package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist/*
var assets embed.FS

func Handler() http.Handler {
	sub, err := fs.Sub(assets, "dist")
	if err != nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if shouldServeIndex(r.URL.Path) {
			content, err := fs.ReadFile(sub, "index.html")
			if err != nil {
				http.Error(w, "index not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(content)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func shouldServeIndex(requestPath string) bool {
	if requestPath == "" || requestPath == "/" {
		return true
	}
	base := path.Base(requestPath)
	return !strings.Contains(base, ".")
}
