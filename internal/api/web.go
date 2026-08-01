package api

import (
	"embed"
	_ "embed"
	"net/http"
	"strings"
)

// mapPage is the browsable view of the catalogue.
//
//go:embed web/map.html
var mapPage []byte

// vendored holds the map library, served by this application rather than from a
// CDN so the only third party a deployment depends on is its tile provider.
//
//go:embed web/vendor/maplibre-gl.js.gz web/vendor/maplibre-gl.css.gz
var vendored embed.FS

// handleMap serves the map.
func (s *Server) handleMap(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Not cached: the page is small and served locally, and a stale copy after
	// an upgrade is a confusing thing to debug.
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(mapPage)
}

// handleVendor serves the map library, pre-compressed.
//
// Gzipped once at build time rather than on every request: the library is 939 KB
// and 242 KB compressed, it never changes between builds, and it is the largest
// thing this server sends.
func (s *Server) handleVendor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	// Only the two files that exist, matched exactly. The path is
	// caller-supplied, and embed.FS would otherwise be walkable.
	switch name {
	case "maplibre-gl.js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case "maplibre-gl.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		http.NotFound(w, r)
		return
	}

	body, err := vendored.ReadFile("web/vendor/" + name + ".gz")
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Encoding", "gzip")
	// Immutable: the library is baked into the binary, so a client that has it
	// never needs to ask again.
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Write(body)
		return
	}
	// A client that cannot take gzip is rare enough to serve correctly rather
	// than quickly: refuse rather than send it bytes it cannot read.
	w.Header().Del("Content-Encoding")
	http.Error(w, "gzip support required", http.StatusNotAcceptable)
}
