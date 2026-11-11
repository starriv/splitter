package webui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// get serves a GET request through mux with the supplied headers and returns
// the recorded response.
func get(mux *http.ServeMux, target string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestPageAndAssets verifies that the embedded page is served with the
// expected content types and restrictive CSP, that every referenced asset is
// embedded and typed, and that no inline script, inline style, event handler
// or HTML-string write exists.
func TestPageAndAssets(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux)

	page := get(mux, "/", nil)
	if page.Code != http.StatusOK || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("/: %d %s", page.Code, page.Header().Get("Content-Type"))
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("CSP = %q", csp)
	}

	// Every asset referenced by the page must be embedded and served with a
	// concrete content type.
	refs := regexp.MustCompile(`(?:href|src)="(/assets/[^"]+)"`).FindAllStringSubmatch(page.Body.String(), -1)
	if len(refs) < 4 {
		t.Fatalf("page references %d assets", len(refs))
	}
	for _, m := range refs {
		rec := get(mux, m[1], nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") == "application/octet-stream" || rec.Body.Len() == 0 {
			t.Errorf("%s: %d %s", m[1], rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	css := get(mux, "/assets/app.css", nil).Body.String()
	for _, m := range regexp.MustCompile(`url\((fonts/[^)]+)\)`).FindAllStringSubmatch(css, -1) {
		rec := get(mux, "/assets/"+m[1], nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "font/woff2" {
			t.Errorf("%s: %d %s", m[1], rec.Code, rec.Header().Get("Content-Type"))
		}
	}

	// The CSP forbids inline script and style, so the page must not contain any.
	html := page.Body.String()
	if regexp.MustCompile(`<script(?:\s[^>]*)?>\s*[^<\s]`).MatchString(html) ||
		strings.Contains(html, "<style") || regexp.MustCompile(`\sstyle="`).MatchString(html) ||
		regexp.MustCompile(`\son[a-z]+="`).MatchString(html) {
		t.Error("index.html contains inline script, style or event handlers, which the CSP blocks")
	}
	// Query names are untrusted: the script builds DOM nodes, never markup.
	if js := get(mux, "/assets/app.js", nil).Body.String(); regexp.MustCompile(`innerHTML\s*=|outerHTML\s*=|insertAdjacentHTML|document\.write`).MatchString(js) {
		t.Error("app.js writes HTML strings into the page")
	}
}

// TestRevalidationAndNotFound verifies the caching contract (ETag, no-cache,
// 304 with an empty body) and that traversal and unknown paths are not
// served.
func TestRevalidationAndNotFound(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux)
	first := get(mux, "/assets/app.js", nil)
	etag := first.Header().Get("ETag")
	if etag == "" || first.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("etag %q cache %q", etag, first.Header().Get("Cache-Control"))
	}
	if rec := get(mux, "/assets/app.js", map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("revalidation: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	for _, p := range []string{"/assets/nope.js", "/assets/", "/assets/../index.html", "/index.html", "/nope"} {
		if rec := get(mux, p, nil); rec.Code == http.StatusOK {
			t.Errorf("%s: served (%d)", p, rec.Code)
		}
	}
}
