// Package server monta as rotas HTTP da aplicação de exemplo.
package server

import (
	"encoding/json"
	"net/http"
)

// NewHandler devolve as rotas da aplicação. Tudo passa pelo rate limiter,
// exceto /healthz, para que health checks não consumam (nem sejam barrados
// pelo) limite de nenhum cliente.
func NewHandler(rateLimit func(http.Handler) http.Handler) http.Handler {
	app := http.NewServeMux()
	app.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"message": "request accepted",
			"path":    r.URL.Path,
		})
	})

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	root.Handle("/", rateLimit(app))
	return root
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
