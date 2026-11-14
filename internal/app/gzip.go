package app

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// gzipHandler compresses dynamic text responses when the client accepts gzip.
// The control page's HTML, CSS, JavaScript and JSON are unminified text; the
// embedded binary serves them directly, and without this a cold load transfers
// several hundred kilobytes that a reverse proxy would otherwise compress.
//
// Already-compressed resources (fonts, images) are passed through untouched.
// The response is wrapped lazily so that handlers which set Content-Type before
// WriteHeader, and handlers that return a bodyless 304, both behave normally.
func gzipHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// acceptsGzip reports whether the request accepts a gzip response, honoring a
// q-value of zero (for example "gzip;q=0").
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		fields := strings.Split(part, ";")
		if strings.TrimSpace(fields[0]) != "gzip" {
			continue
		}
		for _, field := range fields[1:] {
			field = strings.TrimSpace(field)
			if !strings.HasPrefix(field, "q=") {
				continue
			}
			q, err := strconv.ParseFloat(strings.TrimPrefix(field, "q="), 64)
			if err == nil {
				return q > 0
			}
		}
		return true
	}
	return false
}

// compressible reports whether a content type benefits from gzip. Fonts and
// images are already compressed and would only cost CPU.
func compressible(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "javascript") ||
		strings.Contains(ct, "svg") ||
		strings.Contains(ct, "xml")
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	compressed  bool
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	h := w.Header()
	if code >= 200 && code != http.StatusNoContent && code != http.StatusNotModified &&
		h.Get("Content-Encoding") == "" && compressible(h.Get("Content-Type")) {
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		// BestSpeed keeps the first byte of a local console prompt; the JSON is
		// highly redundant and still compresses to a fraction of its size.
		w.gz, _ = gzip.NewWriterLevel(w.ResponseWriter, gzip.BestSpeed)
		w.compressed = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.compressed {
		if _, err := w.gz.Write(p); err != nil {
			return 0, err
		}
		// The handler's contract is the number of input bytes consumed, not
		// the number of compressed bytes written to the socket.
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *gzipResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *gzipResponseWriter) close() {
	if w.gz != nil {
		_ = w.gz.Close()
	}
}
