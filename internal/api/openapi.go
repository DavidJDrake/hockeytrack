package api

import (
	_ "embed"
	"net/http"
	"strconv"
	"time"
)

//go:embed openapi.json
var openAPI []byte

func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(ttlStatic/time.Second)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPI)
}
