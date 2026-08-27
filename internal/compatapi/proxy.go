package compatapi

import (
	"net/http"
	"net/url"
	"path"
	"strings"
)

type relativeLocationWriter struct {
	http.ResponseWriter
	reqPath     string
	wroteHeader bool
}

func (w *relativeLocationWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	rewriteLocationHeader(w.Header(), w.reqPath)
	w.ResponseWriter.WriteHeader(status)
}

func (w *relativeLocationWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *relativeLocationWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *relativeLocationWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func rewriteLocationHeader(h http.Header, reqPath string) {
	loc := h.Get("Location")
	if loc == "" {
		return
	}
	rel := locationToRelative(reqPath, loc)
	if rel != loc {
		h.Set("Location", rel)
	}
}

// Absolute paths like /cam1/index.m3u8 break front proxies that mount MediaMTX under a prefix
// (e.g. /dvr1/<token>/cam1/...), because the browser resolves them from the host root.
func locationToRelative(reqPath, location string) string {
	u, err := url.Parse(location)
	if err != nil {
		return location
	}

	// Already a relative URL (no leading slash, no scheme/host).
	if u.Scheme == "" && u.Host == "" && !strings.HasPrefix(u.Path, "/") {
		return location
	}

	locPath := u.Path
	if locPath == "" {
		return location
	}

	// Absolute URL with host: keep only path+query as candidate for relativization.
	if !strings.HasPrefix(locPath, "/") {
		locPath = "/" + locPath
	}

	dir := path.Dir(reqPath)
	if dir == "." {
		dir = "/"
	}

	var rel string
	switch {
	case locPath == dir:
		rel = "."
	case dir != "/" && strings.HasPrefix(locPath, dir+"/"):
		rel = strings.TrimPrefix(locPath, dir+"/")
	case dir == "/" && strings.HasPrefix(locPath, "/"):
		rel = strings.TrimPrefix(locPath, "/")
	default:
		// Different path tree — keep as-is (still may break prefixes, but uncommon for HLS).
		return location
	}

	if u.RawQuery != "" {
		rel += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		rel += "#" + u.Fragment
	}
	return rel
}
