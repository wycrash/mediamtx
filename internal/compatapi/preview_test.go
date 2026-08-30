package compatapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/test"
)

var testPreviewH264SPS = []byte{
	0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02,
	0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04,
	0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9,
	0x20,
}

func TestExtractPreviewMP4(t *testing.T) {
	var buf bytes.Buffer
	track := &mpegts.Track{Codec: &tscodecs.H264{}}
	w := &mpegts.Writer{W: &buf, Tracks: []*mpegts.Track{track}}
	err := w.Initialize()
	require.NoError(t, err)

	err = w.WriteH264(track, 0, 0, [][]byte{
		testPreviewH264SPS,
		{8}, // PPS
		{5}, // IDR
	})
	require.NoError(t, err)

	dir := t.TempDir()
	tsPath := filepath.Join(dir, "seg.ts")
	require.NoError(t, os.WriteFile(tsPath, buf.Bytes(), 0o644))

	mp4, err := ExtractPreviewMP4(tsPath)
	require.NoError(t, err)
	require.Greater(t, len(mp4), 100)
	require.Equal(t, []byte("ftyp"), mp4[4:8])
}

func TestFindNearest(t *testing.T) {
	idx := NewIndex()
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", "/a.ts", base)
	idx.Add("cam1", "/b.ts", base.Add(10*time.Second))
	idx.Add("cam1", "/c.ts", base.Add(20*time.Second))

	seg, ok := idx.FindNearest("cam1", base.Add(12*time.Second))
	require.True(t, ok)
	require.Equal(t, "/b.ts", seg.Fpath())

	seg, ok = idx.FindNearest("cam1", base.Add(-5*time.Second))
	require.True(t, ok)
	require.Equal(t, "/a.ts", seg.Fpath())
}

func TestFindLatest(t *testing.T) {
	idx := NewIndex()
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", "/a.ts", base)
	idx.Add("cam1", "/c.ts", base.Add(20*time.Second))
	idx.Add("cam1", "/b.ts", base.Add(10*time.Second))

	seg, ok := idx.FindLatest("cam1")
	require.True(t, ok)
	require.Equal(t, "/c.ts", seg.Fpath())
}

func TestPreviewCivilTime(t *testing.T) {
	ts := previewCivilTime(2025, 9, 21, 15, 0, 0, 180)
	require.Equal(t, int64(1758456000), ts.Unix())
}

func TestPreviewRegexp(t *testing.T) {
	m := previewRegexp.FindStringSubmatch("cam4/2025/09/21/15/00/00.mp4")
	require.NotNil(t, m)
	require.Equal(t, "cam4", m[1])

	m = previewRegexp.FindStringSubmatch("cam4/2025/09/21/15/00/00-preview.mp4")
	require.NotNil(t, m)
	require.Equal(t, "cam4", m[1])
}

func TestPreviewUnixRegexp(t *testing.T) {
	m := previewUnixRegexp.FindStringSubmatch("cam1/1786648428-preview.mp4")
	require.NotNil(t, m)
	require.Equal(t, "cam1", m[1])
	require.Equal(t, "1786648428", m[2])

	require.Nil(t, previewUnixRegexp.FindStringSubmatch("cam1/preview.mp4"))
	require.Nil(t, previewUnixRegexp.FindStringSubmatch("cam1/2025/09/21/15/00/00-preview.mp4"))
}

func TestPreviewHead(t *testing.T) {
	gin.SetMode(gin.TestMode)

	idx := NewIndex()
	ts := previewCivilTime(2026, 8, 27, 22, 12, 0, 0)
	idx.Add("cam6", "/rec/cam6/a.ts", ts)

	s := &Server{
		PathConfs: map[string]*conf.Path{
			"cam6": {Name: "cam6"},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
		Index:       idx,
	}
	r := gin.New()
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodHead, "/cam6/2026/08/27/22/12/00-preview.mp4?token=abc", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "video/mp4", w.Header().Get("Content-Type"))
	require.Empty(t, w.Body.Bytes())

	s2 := &Server{
		PathConfs: map[string]*conf.Path{
			"cam6": {Name: "cam6"},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
		Index:       NewIndex(),
	}
	r2 := gin.New()
	r2.NoRoute(s2.onRequest)
	req = httptest.NewRequest(http.MethodHead, "/cam6/2026/08/27/22/12/00-preview.mp4", nil)
	w = httptest.NewRecorder()
	r2.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)

	req = httptest.NewRequest(http.MethodPost, "/cam6/2026/08/27/22/12/00-preview.mp4", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusMethodNotAllowed, w.Code)
}
