package compatapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

func TestLocationToRelative(t *testing.T) {
	require.Equal(t,
		"index.m3u8?cookieCheck=1",
		locationToRelative("/cam1/index.m3u8", "/cam1/index.m3u8?cookieCheck=1"),
	)
	require.Equal(t,
		"index.m3u8?cookieCheck=1",
		locationToRelative("/cam1/index.m3u8", "http://127.0.0.1:8888/cam1/index.m3u8?cookieCheck=1"),
	)
	require.Equal(t,
		"index.m3u8?cookieCheck=1",
		locationToRelative("/cam1/index.m3u8", "index.m3u8?cookieCheck=1"),
	)
	require.Equal(t,
		"seg.mp4",
		locationToRelative("/cam1/index.m3u8", "/cam1/seg.mp4"),
	)
}

func TestIsWHEPPath(t *testing.T) {
	require.True(t, isWHEPPath("/cam226/whep"))
	require.True(t, isWHEPPath("/group/cam226/whep"))
	require.True(t, isWHEPPath("/cam226/whep/6ba7b810-9dad-11d1-80b4-00c04fd430c8"))
	require.False(t, isWHEPPath("/cam226/whepfoo"))
	require.False(t, isWHEPPath("/cam226/index.m3u8"))
	require.False(t, isWHEPPath("/whep"))
}

func TestServeWHEPUsesHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotMethod, gotPath string
	s := &Server{
		WebRTCHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			w.Header().Set("Location", "/cam226/whep/6ba7b810-9dad-11d1-80b4-00c04fd430c8")
			w.WriteHeader(http.StatusCreated)
		}),
		Parent: test.NilLogger,
	}

	r := gin.New()
	r.Use(s.middlewarePreflightRequests)
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodPost, "/cam226/whep", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "/cam226/whep", gotPath)
	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, "whep/6ba7b810-9dad-11d1-80b4-00c04fd430c8", w.Header().Get("Location"))

	req = httptest.NewRequest(http.MethodPatch, "/cam226/whep/6ba7b810-9dad-11d1-80b4-00c04fd430c8", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.MethodPatch, gotMethod)
	require.Equal(t, http.StatusCreated, w.Code)

	req = httptest.NewRequest(http.MethodPost, "/cam226/whep", nil)
	s.WebRTCHandler = nil
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadGateway, w.Code)
}

func TestServeLiveUsesHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath string
	s := &Server{
		HLSHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Location", "/cam1/index.m3u8?cookieCheck=1")
			w.WriteHeader(http.StatusFound)
		}),
		Parent: test.NilLogger,
	}

	r := gin.New()
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodGet, "/cam1/index.m3u8", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, "/cam1/index.m3u8", gotPath)
	require.Equal(t, http.StatusFound, w.Code)
	require.Equal(t, "index.m3u8?cookieCheck=1", w.Header().Get("Location"))
}

func TestRewriteLivePlaylistPath(t *testing.T) {
	require.Equal(t, "/cam1/index.m3u8", rewriteLivePlaylistPath("/cam1/index.m3u8"))
	require.Equal(t, "/cam1/index.m3u8", rewriteLivePlaylistPath("/cam1/index.fmp4.m3u8"))
	require.Equal(t, "/cam1/index.m3u8", rewriteLivePlaylistPath("/cam1/video.m3u8"))
	require.Equal(t, "/cam1/index.m3u8", rewriteLivePlaylistPath("/cam1/video.fmp4.m3u8"))
	require.Equal(t, "/group/cam1/index.m3u8", rewriteLivePlaylistPath("/group/cam1/video.m3u8"))
	require.Equal(t, "/cam1/index-1000-60.m3u8", rewriteLivePlaylistPath("/cam1/index-1000-60.m3u8"))
}

func TestServeLiveRewritesFMP4Playlist(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath string
	s := &Server{
		HLSHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}),
		Parent: test.NilLogger,
	}

	r := gin.New()
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodGet, "/cam1/index.fmp4.m3u8", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, "/cam1/index.m3u8", gotPath)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestServeLiveRewritesVideoPlaylist(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath string
	s := &Server{
		HLSHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}),
		Parent: test.NilLogger,
	}

	r := gin.New()
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodGet, "/cam1/video.m3u8", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, "/cam1/index.m3u8", gotPath)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestServeLiveWithoutHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{Parent: test.NilLogger}
	r := gin.New()
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodGet, "/cam1/index.m3u8", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadGateway, w.Code)
}
