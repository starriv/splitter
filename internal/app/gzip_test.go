package app

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// gunzip decodes a gzipped response body.
func gunzip(t *testing.T, r io.Reader) string {
	t.Helper()
	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(b)
}

// TestAdminGzip verifies that the admin handler compresses compressible text
// when asked, passes already-compressed assets through, and honors an explicit
// gzip refusal. The embedded page is served directly in development and in
// front of Caddy, so both paths must behave.
func TestAdminGzip(t *testing.T) {
	e := start(t)
	h := e.app.AdminHandler()
	gz := map[string]string{"Accept-Encoding": "gzip"}

	// JSON API: the largest dynamic response.
	rec := call(t, h, "GET", "/api/v1/state", "", gz)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("state: %d encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Errorf("Vary = %q", vary)
	}
	var st State
	if err := json.Unmarshal([]byte(gunzip(t, rec.Body)), &st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if st.Status.Version != "test" {
		t.Errorf("compressed state did not decode: %+v", st.Status)
	}

	// The page is text/html and must travel gzipped as well.
	rec = call(t, h, "GET", "/", "", gz)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("page: %d encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	if html := gunzip(t, rec.Body); !strings.Contains(html, "<!doctype html>") {
		t.Errorf("page body does not look like HTML: %q", html[:min(len(html), 80)])
	}

	// The Prometheus exposition endpoint is text; promhttp uses io.Writer
	// directly, so this also covers the Write-contract fix.
	rec = call(t, h, "GET", "/metrics", "", gz)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("metrics: %d encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	if metrics := gunzip(t, rec.Body); !strings.Contains(metrics, "splitter_build_info") {
		t.Errorf("gzipped metrics body is incomplete")
	}

	// A WOFF2 font is already compressed; wrapping it again only costs CPU.
	rec = call(t, h, "GET", "/assets/fonts/inter-latin-wght-normal.woff2", "", gz)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("font: %d encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}

	// gzip;q=0 is an explicit refusal; a positive q-value still compresses.
	rec = call(t, h, "GET", "/api/v1/state", "", map[string]string{"Accept-Encoding": "gzip;q=0"})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("q=0: %d encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	rec = call(t, h, "GET", "/api/v1/state", "", map[string]string{"Accept-Encoding": "gzip;q=0.5"})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("q=0.5: %d encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
}

// TestAdminGzipRevalidation verifies that a 304 remains bodyless and is not
// wrapped in a gzip stream even though its content type is compressible.
func TestAdminGzipRevalidation(t *testing.T) {
	e := start(t)
	h := e.app.AdminHandler()
	first := call(t, h, "GET", "/assets/app.js", "", map[string]string{"Accept-Encoding": "gzip"})
	etag := first.Header().Get("ETag")
	if etag == "" || first.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("first: encoding %q etag %q", first.Header().Get("Content-Encoding"), etag)
	}
	rec := call(t, h, "GET", "/assets/app.js", "", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag})
	if rec.Code != http.StatusNotModified || rec.Header().Get("Content-Encoding") != "" || rec.Body.Len() != 0 {
		t.Errorf("revalidation: %d encoding %q bytes %d", rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.Len())
	}
}
