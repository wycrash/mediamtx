package compatapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/test"
)

func tokenAuthManager() *auth.Manager {
	return &auth.Manager{
		Method: conf.AuthMethodInternal,
		InternalUsers: []conf.AuthInternalUser{{
			User: "token",
			Pass: "secret",
			Permissions: []conf.AuthInternalUserPermission{{
				Action: conf.AuthActionPlayback,
			}},
		}},
	}
}

func TestArchivePlaylistForwardsTokenToSegments(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	segName := "2026-08-31-21-59-51-1788202791-801076.mp4"
	fpath := filepath.Join(dir, segName)
	writeArchiveTestSegment(t, fpath)

	start := time.Unix(1788202791, 0).UTC()
	idx := NewIndex()
	idx.Add("cam1", fpath, start)
	idx.SetFMP4Meta("cam1", fpath, fmp4SegMeta{
		Duration:  2 * time.Second,
		MoofCount: 1,
		Ready:     true,
	})

	s := &Server{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:                  "cam1",
				RecordFormat:          conf.RecordFormatFMP4,
				RecordSegmentDuration: conf.Duration(10 * time.Second),
			},
		},
		AuthManager: tokenAuthManager(),
		Parent:      test.NilLogger,
		Index:       idx,
	}
	r := gin.New()
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodGet,
		"/cam1/index-1788202791-10.m3u8?token=secret", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	body, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), `#EXT-X-MAP:URI="`+segName+`?hls=init&token=secret"`)
	require.Contains(t, string(body), segName+`?hls=media&sn=0&td=0&token=secret`)

	req = httptest.NewRequest(http.MethodGet,
		"/cam1/"+segName+"?hls=media&sn=0&td=0&token=secret", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "video/mp4", w.Header().Get("Content-Type"))

	req = httptest.NewRequest(http.MethodGet,
		"/cam1/"+segName+"?hls=media&sn=0&td=0", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestUnknownMP4DoesNotAuthBeforeLiveFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	s := &Server{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:         "cam1",
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		AuthManager: tokenAuthManager(),
		Parent:      test.NilLogger,
		Index:       NewIndex(),
		HLSHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}),
	}
	r := gin.New()
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodGet, "/cam1/seg0.mp4", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusTeapot, w.Code)
}

func TestPlaylistAuthQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("query token", func(t *testing.T) {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/cam1/index.m3u8?token=secret&hls=1", nil)
		require.Equal(t, "token=secret", playlistAuthQuery(ctx))
	})

	t.Run("bearer", func(t *testing.T) {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/cam1/index.m3u8", nil)
		ctx.Request.Header.Set("Authorization", "Bearer secret")
		require.Equal(t, "token=secret", playlistAuthQuery(ctx))
	})
}
