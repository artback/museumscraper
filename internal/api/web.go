package api

import (
	_ "embed"
	"net/http"
)

// mapPage is the browsable view of the catalogue.
//
// Embedded rather than served from disk so the binary stays a single file, and
// deliberately self-contained: no map tiles, no CDN, no fonts. The API is the
// only thing it talks to, which means it works on a laptop with no network as
// readily as anywhere else. It can afford to have no basemap because at
// 154,000 placed museums the points draw the coastlines themselves.
//
//go:embed web/map.html
var mapPage []byte

// handleMap serves the map.
func (s *Server) handleMap(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Not cached: the page is small and served locally, and a stale copy after
	// an upgrade is a confusing thing to debug.
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(mapPage)
}
