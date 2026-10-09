package main

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web/*
var webFiles embed.FS

func newRouter(itemHandler *ItemHandler, media *MediaHandler, ai *AIHandler, notifications *NotificationHandler) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/register", handleRegister)
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/logout", handleLogout)
	mux.HandleFunc("/api/me", handleMe)

	mux.HandleFunc("/api/items", itemHandler.Collection)
	mux.HandleFunc("/api/items/{id}", itemHandler.ByID)
	mux.HandleFunc("/api/items/{id}/status", itemHandler.ChangeStatus)
	mux.HandleFunc("/api/media", media.Upload)
	mux.HandleFunc("/api/media/{id}/content", media.Content)
	mux.HandleFunc("/api/config", ai.Config)
	mux.HandleFunc("/api/ai/extract", ai.Extract)
	mux.HandleFunc("/api/ai/jobs/{id}", ai.Job)
	mux.HandleFunc("/api/notifications", notifications.List)
	mux.HandleFunc("/api/notifications/{id}/read", notifications.Read)

	mux.HandleFunc("/api/health", handleHealth)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, Response{Code: 404, Message: "接口不存在"})
	})
	assets, _ := fs.Sub(webFiles, "web")
	mux.Handle("/", http.FileServer(http.FS(assets)))

	return mux
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
