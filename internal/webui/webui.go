// Package webui serves the web control page: one HTML document with its
// stylesheet, scripts and fonts, embedded in the binary so that the distroless
// image requires no additional files. The page is plain HTML, CSS and
// JavaScript, so the image build requires no bundler and no Node.js toolchain.
//
// The page communicates only with the JSON API in internal/app (same origin),
// which the Content-Security-Policy below enforces.
//
// The fonts under static/assets/fonts are the Latin subsets of Geist, Geist
// Mono and Inter (SIL Open Font License 1.1; see OFL.txt in that directory).
package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var static embed.FS

// Content types by extension. mime.TypeByExtension is not used because it
// consults the host's mime.types file, which the distroless image does not
// provide, and its built-in table has no entry for woff2.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
}

// The page loads no external resources and runs no inline script or style, so
// query names displayed on it, which any device on the network can choose,
// cannot become executable markup.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; " +
	"img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

type asset struct {
	data  []byte
	ctype string
	etag  string
}

// Register adds the control page at / and its files under /assets/ to mux.
func Register(mux *http.ServeMux) {
	assets := load()
	serve := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			a, ok := assets[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			h := w.Header()
			h.Set("Content-Type", a.ctype)
			h.Set("ETag", a.etag)
			// Always revalidate: these files change together with the binary, and a 304
			// response costs one round trip on a local network.
			h.Set("Cache-Control", "no-cache")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", csp)
			if r.Header.Get("If-None-Match") == a.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = w.Write(a.data)
		}
	}
	mux.HandleFunc("GET /{$}", serve("index.html"))
	mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
		serve(strings.TrimPrefix(path.Clean(r.URL.Path), "/"))(w, r)
	})
}

func load() map[string]asset {
	assets := map[string]asset{}
	root, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	err = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		ctype, ok := contentTypes[path.Ext(p)]
		if !ok {
			ctype = "application/octet-stream"
		}
		sum := sha256.Sum256(data)
		assets[p] = asset{data: data, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		return nil
	})
	if err != nil {
		panic(err)
	}
	return assets
}
