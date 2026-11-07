package middleware

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	trusted, err := ParseTrustedProxies("10.0.0.0/8, 172.16.0.5")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		trusted bool
		remote  string
		xff     []string
		xRealIP string
		want    string
	}{
		{name: "remote addr only", remote: "203.0.113.7:5555", want: "203.0.113.7"},
		{name: "headers ignored without trusted proxies", remote: "203.0.113.7:5555", xff: []string{"1.1.1.1"}, xRealIP: "2.2.2.2", want: "203.0.113.7"},
		{name: "headers ignored from untrusted peer", trusted: true, remote: "203.0.113.7:5555", xff: []string{"1.1.1.1"}, xRealIP: "2.2.2.2", want: "203.0.113.7"},
		{name: "xff from trusted proxy", trusted: true, remote: "10.1.2.3:80", xff: []string{"198.51.100.20"}, want: "198.51.100.20"},
		{name: "spoofed leftmost entries are ignored", trusted: true, remote: "10.1.2.3:80", xff: []string{"6.6.6.6, 198.51.100.20"}, want: "198.51.100.20"},
		{name: "chain of trusted proxies", trusted: true, remote: "10.1.2.3:80", xff: []string{"198.51.100.20, 172.16.0.5", "10.9.9.9"}, want: "198.51.100.20"},
		{name: "every hop trusted uses leftmost", trusted: true, remote: "10.1.2.3:80", xff: []string{"10.5.5.5, 10.6.6.6"}, want: "10.5.5.5"},
		{name: "garbage hop stops the walk", trusted: true, remote: "10.1.2.3:80", xff: []string{"6.6.6.6, not-an-ip, 10.6.6.6"}, want: "10.6.6.6"},
		{name: "x-real-ip when no xff", trusted: true, remote: "10.1.2.3:80", xRealIP: "198.51.100.30", want: "198.51.100.30"},
		{name: "invalid x-real-ip falls back to remote", trusted: true, remote: "10.1.2.3:80", xRealIP: "nope", want: "10.1.2.3"},
		{name: "xff wins over x-real-ip", trusted: true, remote: "10.1.2.3:80", xff: []string{"198.51.100.20"}, xRealIP: "198.51.100.30", want: "198.51.100.20"},
		{name: "ipv6 remote", remote: "[2001:db8::1]:443", want: "2001:db8::1"},
		{name: "ipv4-mapped ipv6 is normalised", remote: "[::ffff:203.0.113.7]:443", want: "203.0.113.7"},
		{name: "xff hop with port", trusted: true, remote: "10.1.2.3:80", xff: []string{"198.51.100.20:4711"}, want: "198.51.100.20"},
		{name: "xff ipv6 in brackets", trusted: true, remote: "10.1.2.3:80", xff: []string{"[2001:db8::7]"}, want: "2001:db8::7"},
		{name: "remote addr without port", remote: "203.0.113.7", want: "203.0.113.7"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if tc.xRealIP != "" {
				r.Header.Set("X-Real-IP", tc.xRealIP)
			}

			resolver := NewIPResolver(nil)
			if tc.trusted {
				resolver = NewIPResolver(trusted)
			}
			if got := resolver.ClientIP(r); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies(" 10.0.0.0/8 ,, 192.168.1.10, ::1, 172.16.5.4/12 ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "192.168.1.10/32", "::1/128", "172.16.0.0/12"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("prefix %d = %s, want %s", i, got[i], want[i])
		}
	}

	for _, bad := range []string{"10.0.0.0/33", "banana", "10.0.0.1/abc"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}
