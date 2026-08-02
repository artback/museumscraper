package api

import (
	"embed"
	_ "embed"
	"net/http"
	"path"
	"strings"
)

// mapPage is the browsable view of the catalogue.
//
//go:embed web/map.html
var mapPage []byte

// assets holds the page's own stylesheet and modules. They live beside map.html
// rather than inside it because the page outgrew a single file: one stylesheet
// and one module per concern is reviewable, and a diff then says which concern
// changed. They are still only static files — there is no build step, and the
// modules are the source the browser runs.
//
//go:embed web/assets
var assets embed.FS

// vendored holds the map library, served by this application rather than from a
// CDN so the only third party a deployment depends on is its tile provider.
//
//go:embed web/vendor/maplibre-gl.js.gz web/vendor/maplibre-gl.css.gz
var vendored embed.FS

// assetTypes is the set of files handleAsset will serve, by extension. A map
// rather than a switch on the whole name so adding a module means adding a
// file, but an extension this server has no content type for is still a 404
// rather than a guess.
var assetTypes = map[string]string{
	".css": "text/css; charset=utf-8",
	".js":  "application/javascript; charset=utf-8",
}

// contentPolicy is what the page is allowed to load and run.
//
// Worth stating because the page renders text this project did not write:
// museum names come from Wikidata and OpenStreetMap, and exhibition titles and
// links are scraped from museums' own websites. The page is careful with them —
// it builds nodes rather than concatenating markup, and refuses any link that
// is not http(s) — but a policy is the backstop for the case where it is not.
//
// script-src needs no 'unsafe-inline': the page's own code is served as modules
// from this origin, so nothing inline is executable. Styles do need it, because
// both MapLibre and this page set style attributes on elements they create.
const contentPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob: https://basemaps.cartocdn.com; " +
	// The tile host belongs in connect-src as well as img-src: MapLibre fetches
	// raster tiles with fetch() so it can decode them off the main thread, and
	// a policy that lists them only as images blocks every one of them — which
	// leaves a globe with no surface and nothing on screen to say why.
	"connect-src 'self' https://basemaps.cartocdn.com https://demotiles.maplibre.org; " +
	"worker-src blob:; child-src blob:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// handleMap serves the map.
func (s *Server) handleMap(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", contentPolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Not cached: the page is small and served locally, and a stale copy after
	// an upgrade is a confusing thing to debug.
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(mapPage)
}

// handleAsset serves the page's stylesheet and modules.
//
// The name is caller-supplied, so it is checked rather than trusted: embed.FS
// is walkable, and a path with a slash or a dot-dot in it would otherwise read
// whatever the binary happens to carry.
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}

	contentType, ok := assetTypes[path.Ext(name)]
	if !ok {
		http.NotFound(w, r)
		return
	}

	body, err := assets.ReadFile("web/assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", contentType)
	// Not cached, for the same reason the page is not: these are small, served
	// locally, and a stale module after an upgrade is a confusing thing to
	// debug — the more so now that the page is split across several of them and
	// a half-updated set is a state that cannot happen any other way.
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(body)
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
