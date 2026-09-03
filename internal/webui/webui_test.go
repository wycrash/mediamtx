package webui

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSafeRel(t *testing.T) {
	ok := func(in, want string) {
		t.Helper()
		got, good := SafeRel(in)
		require.True(t, good)
		require.Equal(t, want, got)
	}
	bad := func(in string) {
		t.Helper()
		_, good := SafeRel(in)
		require.False(t, good)
	}

	ok("js/MediaMtxDvrPlayer.js", "js/MediaMtxDvrPlayer.js")
	ok("js/../css/player.css", "css/player.css")
	ok("/css/player.css", "css/player.css")
	bad("")
	bad(".")
	bad("..")
	bad("../webui.go")
	bad(".keep")
	bad("js/.hidden")
}

func TestServeFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fsys := fstest.MapFS{
		"embed.html": &fstest.MapFile{Data: []byte("<html>player</html>")},
		"js/app.js":  &fstest.MapFile{Data: []byte("export default 1")},
	}

	r := gin.New()
	r.GET("/lib/dvrplayer/*filepath", func(ctx *gin.Context) {
		ServeFile(ctx, fsys, ctx.Param("filepath"), "max-age=3600")
	})

	req := httptest.NewRequest(http.MethodGet, "/lib/dvrplayer/js/app.js", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/javascript; charset=utf-8", w.Header().Get("Content-Type"))
	require.Equal(t, "max-age=3600", w.Header().Get("Cache-Control"))
	body, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	require.Equal(t, "export default 1", string(body))

	req = httptest.NewRequest(http.MethodGet, "/lib/dvrplayer/.keep", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)

	_, err = ReadFile(fsys, "missing.js")
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestDvrPlayerGenerated(t *testing.T) {
	data, err := fs.ReadFile(DvrPlayer(), "embed.html")
	if err != nil {
		t.Skip("dvr player not copied; run go generate ./internal/webui")
	}
	require.Contains(t, string(data), "MediaMtxDvrPlayer")
}

func TestServeSPA(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fsys := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>spa</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("1")},
	}
	r := gin.New()
	r.NoRoute(func(ctx *gin.Context) {
		ServeSPA(ctx, fsys, strings.TrimPrefix(ctx.Request.URL.Path, "/admin/"))
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "spa")

	req = httptest.NewRequest(http.MethodGet, "/admin/paths", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "spa")

	req = httptest.NewRequest(http.MethodGet, "/admin/assets/app.js", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	req = httptest.NewRequest(http.MethodGet, "/admin/missing.js", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestAdminGenerated(t *testing.T) {
	data, err := fs.ReadFile(Admin(), "index.html")
	if err != nil {
		t.Skip("admin UI not copied; run go generate ./internal/webui")
	}
	require.Contains(t, string(data), "<html")
}
