package compatapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
)

func isHLSProxyClientGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, http.ErrAbortHandler) ||
		errors.Is(err, net.ErrClosed) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "context canceled") ||
		strings.Contains(s, "request canceled")
}

// rewriteHLSProxyResponse rewrites absolute-path Location headers to relative ones.
// Absolute paths like /cam1/index.m3u8 break front proxies that mount MediaMTX under a prefix
// (e.g. /dvr1/<token>/cam1/...), because the browser resolves them from the host root.
func rewriteHLSProxyResponse(resp *http.Response) error {
	if resp == nil || resp.Request == nil {
		return nil
	}

	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil
	}

	rel := locationToRelative(resp.Request.URL.Path, loc)
	if rel != loc {
		resp.Header.Set("Location", rel)
	}
	return nil
}

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
