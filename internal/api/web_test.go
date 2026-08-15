package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestAsset_ServesTheStylesheetAndModules(t *testing.T) {
	for _, tc := range []struct{ file, wantType string }{
		{"app.css", "text/css; charset=utf-8"},
		{"app.js", "application/javascript; charset=utf-8"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			rec := get(t, &fakeCatalogue{}, "/map/assets/"+tc.file)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.wantType {
				t.Errorf("content type = %q, want %q", got, tc.wantType)
			}
			if rec.Body.Len() == 0 {
				t.Error("body is empty")
			}
		})
	}
}

// The name comes from the URL and embed.FS is walkable, so anything that is not
// an asset must be a 404 rather than a read of whatever the binary carries.
func TestAsset_RefusesAnythingButAnAsset(t *testing.T) {
	for _, name := range []string{
		"vendor",     // a directory
		"map.html",   // a real embedded file, wrong extension
		"app.css.gz", // an extension with no content type
		"",           // nothing at all
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(t, &fakeCatalogue{}, "/map/assets/"+name)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (body: %.80s)", rec.Code, rec.Body)
			}
		})
	}
}

// A path that climbs out of the assets directory must never return a file.
// Asserted as "not 200" rather than "404" because ServeMux cleans the path and
// redirects before the handler sees it: the redirect is the refusal, and
// pinning the exact status would be a test of the standard library.
func TestAsset_RefusesTraversal(t *testing.T) {
	for _, name := range []string{"../map.html", "..%2fmap.html", "../../go.mod"} {
		t.Run(name, func(t *testing.T) {
			rec := get(t, &fakeCatalogue{}, "/map/assets/"+name)
			if rec.Code == http.StatusOK {
				t.Errorf("served a file for %q: %.80s", name, rec.Body)
			}
		})
	}
}

// The page loads its stylesheet and entry module by URL; if either name stops
// matching a file that is served, the map is a blank screen with a console
// error and nothing else says so.
func TestMapPage_ReferencesAssetsThatExist(t *testing.T) {
	rec := get(t, &fakeCatalogue{}, "/map")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	page := rec.Body.String()

	for _, want := range []string{"/map/assets/app.css", "/map/assets/app.js"} {
		if !strings.Contains(page, want) {
			t.Errorf("page does not reference %s", want)
			continue
		}
		if got := get(t, &fakeCatalogue{}, want); got.Code != http.StatusOK {
			t.Errorf("page references %s, which serves %d", want, got.Code)
		}
	}
}
