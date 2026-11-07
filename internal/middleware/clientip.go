package middleware

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// IPResolver descobre o IP do cliente de uma requisição.
//
// X-Forwarded-For e X-Real-IP podem ser forjados por qualquer cliente, então
// só são lidos quando a conexão vem de um proxy confiável (TrustedProxies).
// Nesse caso o X-Forwarded-For é percorrido da direita para a esquerda,
// pulando os proxies confiáveis, e o primeiro endereço não confiável é o
// cliente: entradas mais à esquerda foram escritas pelo próprio cliente e
// são ignoradas. Sem X-Forwarded-For, usa-se X-Real-IP. Em qualquer outro
// caso vale o endereço da conexão (RemoteAddr).
type IPResolver struct {
	trusted []netip.Prefix
}

// NewIPResolver cria um resolver. Sem proxies confiáveis, os headers são
// sempre ignorados.
func NewIPResolver(trustedProxies []netip.Prefix) *IPResolver {
	return &IPResolver{trusted: trustedProxies}
}

// ParseTrustedProxies converte uma lista de CIDRs ou IPs ("10.0.0.0/8,
// 192.168.1.10") em prefixos.
func ParseTrustedProxies(list string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			p, err := netip.ParsePrefix(item)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy %q: %w", item, err)
			}
			prefixes = append(prefixes, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: %w", item, err)
		}
		addr = addr.Unmap()
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return prefixes, nil
}

// ClientIP devolve o IP do cliente em forma canônica.
func (r *IPResolver) ClientIP(req *http.Request) string {
	host := req.RemoteAddr
	if h, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		host = h
	}
	remote, err := parseAddr(host)
	if err != nil {
		return host
	}
	if !r.isTrusted(remote) {
		return remote.String()
	}

	if hops := forwardedFor(req.Header); len(hops) > 0 {
		client := remote
		for i := len(hops) - 1; i >= 0; i-- {
			addr, err := parseAddr(hops[i])
			if err != nil {
				break // cadeia corrompida: não confia em nada à esquerda
			}
			client = addr
			if !r.isTrusted(addr) {
				break
			}
		}
		return client.String()
	}

	if addr, err := parseAddr(req.Header.Get("X-Real-IP")); err == nil {
		return addr.String()
	}
	return remote.String()
}

func (r *IPResolver) isTrusted(addr netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func forwardedFor(h http.Header) []string {
	var hops []string
	for _, value := range h.Values("X-Forwarded-For") {
		for _, hop := range strings.Split(value, ",") {
			if hop = strings.TrimSpace(hop); hop != "" {
				hops = append(hops, hop)
			}
		}
	}
	return hops
}

func parseAddr(s string) (netip.Addr, error) {
	s = strings.TrimSpace(s)
	// Aceita "[::1]" e "1.2.3.4:5678", formas que alguns proxies usam.
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, err
	}
	return addr.Unmap().WithZone(""), nil
}
