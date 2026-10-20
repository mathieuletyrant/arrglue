package main

import (
	_ "embed"
	"net/http"
)

// ui.html is the whole web UI: it reads /v1/state and posts to /v1/scan and /v1/apply.
//
//go:embed ui.html
var uiPage []byte

func (s *server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(uiPage)
}
