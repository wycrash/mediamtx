package compatapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/test"
)

func testDvrPlayerFS() fstest.MapFS {
	return fstest.MapFS{
		"embed.html": &fstest.MapFile{Data: []byte("<html>MediaMtxDvrPlayer</html>")},
		"js/MediaMtxDvrPlayer.js": &fstest.MapFile{
			Data: []byte("export default { load() {} }"),
		},
	}
}

func TestEmbedHTML(t *testing.T) {
	gin.SetMode(gin.TestMode)

	s := &Server{
		PathConfs: map[string]*conf.Path{
			"cam1":       {Name: "cam1"},
			"group/cam1": {Name: "group/cam1"},
		},
		DvrPlayer: testDvrPlayerFS(),
		Parent:    test.NilLogger,
	}
	r := gin.New()
	r.Use(s.middlewareSession)
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodGet, "/cam1/embed.html?dvr=true&autoplay=false", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))
	body, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "MediaMtxDvrPlayer")
	list, err := s.APISessionsList()
	require.NoError(t, err)
	require.Empty(t, list.Items)

	req = httptest.NewRequest(http.MethodHead, "/cam1/embed.html", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	req = httptest.NewRequest(http.MethodGet, "/group/cam1/embed.html", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	req = httptest.NewRequest(http.MethodGet, "/missing/embed.html", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDvrPlayerAssets(t *testing.T) {
	gin.SetMode(gin.TestMode)

	s := &Server{
		DvrPlayer: testDvrPlayerFS(),
		Parent:    test.NilLogger,
	}
	r := gin.New()
	r.Use(s.middlewareSession)
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodGet, "/lib/dvrplayer/js/MediaMtxDvrPlayer.js", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/javascript; charset=utf-8", w.Header().Get("Content-Type"))
	require.Equal(t, "max-age=3600", w.Header().Get("Cache-Control"))
	body, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "export default")
	list, err := s.APISessionsList()
	require.NoError(t, err)
	require.Empty(t, list.Items)

	req = httptest.NewRequest(http.MethodGet, "/lib/dvrplayer/embed.html", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	req = httptest.NewRequest(http.MethodGet, "/lib/dvrplayer/.keep", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)

	req = httptest.NewRequest(http.MethodGet, "/lib/dvrplayer/missing.js", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestSkipCompatSession(t *testing.T) {
	require.True(t, skipCompatSession("/lib/dvrplayer/js/app.js"))
	require.True(t, skipCompatSession("/cam1/embed.html"))
	require.True(t, skipCompatSession("/group/cam1/embed.html"))
	require.False(t, skipCompatSession("/cam1/index.m3u8"))
	require.False(t, skipCompatSession("/cam1/info.json"))
}
