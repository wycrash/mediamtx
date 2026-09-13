package compatapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/test"
)

func previewPartMarker(i int) []byte {
	return []byte{0xDE, 0xAD, 0xBE, 0xEF, byte(i), 0xAA, 0x55, 0x00}
}

func requirePreviewMarker(t *testing.T, mp4 []byte, want int, not ...int) {
	t.Helper()
	require.Truef(t, bytes.Contains(mp4, previewPartMarker(want)), "missing marker %d", want)
	for _, i := range not {
		require.Falsef(t, bytes.Contains(mp4, previewPartMarker(i)), "unexpected marker %d", i)
	}
}

func writeFMP4PreviewParts(t *testing.T, fpath string, n, idrEvery int) {
	t.Helper()
	f, err := os.Create(fpath)
	require.NoError(t, err)
	defer f.Close()

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{
				ID:        1,
				TimeScale: 1000,
				Codec: &mcodecs.H264{
					SPS: test.FormatH264.SPS,
					PPS: test.FormatH264.PPS,
				},
			},
		},
	}
	require.NoError(t, init.Marshal(f))
	for i := 0; i < n; i++ {
		payload := append([]byte{0, 0, 0, 8, 5}, previewPartMarker(i)...)
		part := fmp4.Part{
			SequenceNumber: uint32(i),
			Tracks: []*fmp4.PartTrack{{
				ID:       1,
				BaseTime: uint64(i * 1000),
				Samples: []*fmp4.Sample{{
					Duration:        1000,
					IsNonSyncSample: idrEvery > 0 && i%idrEvery != 0,
					Payload:         payload,
				}},
			}},
		}
		require.NoError(t, part.Marshal(f))
	}
}

func TestPreviewFMP4PartAt(t *testing.T) {
	parts := []fmp4MediaPart{
		{Off: 100, PTSStart: 0, HasIDR: true},
		{Off: 200, PTSStart: time.Second, HasIDR: false},
		{Off: 300, PTSStart: 2 * time.Second, HasIDR: true},
		{Off: 400, PTSStart: 3 * time.Second, HasIDR: false},
		{Off: 500, PTSStart: 4 * time.Second, HasIDR: true},
	}

	p, ok := previewFMP4PartAt(parts, 0)
	require.True(t, ok)
	require.Equal(t, int64(100), p.Off)

	p, ok = previewFMP4PartAt(parts, 3500*time.Millisecond)
	require.True(t, ok)
	require.Equal(t, int64(300), p.Off)

	p, ok = previewFMP4PartAt(parts, 4*time.Second)
	require.True(t, ok)
	require.Equal(t, int64(500), p.Off)

	p, ok = previewFMP4PartAt(parts, time.Hour)
	require.True(t, ok)
	require.Equal(t, int64(500), p.Off)

	late := []fmp4MediaPart{
		{Off: 10, PTSStart: 0, HasIDR: false},
		{Off: 20, PTSStart: 2 * time.Second, HasIDR: true},
	}
	p, ok = previewFMP4PartAt(late, time.Second)
	require.True(t, ok)
	require.Equal(t, int64(20), p.Off)

	_, ok = previewFMP4PartAt([]fmp4MediaPart{{Off: 1, HasIDR: false}}, time.Second)
	require.False(t, ok)
}

func TestExtractPreviewFMP4At(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seg.mp4")
	writeFMP4PreviewParts(t, path, 6, 2)

	mp4, err := ExtractPreviewFMP4(path, 0)
	require.NoError(t, err)
	requirePreviewMarker(t, mp4, 0, 2)

	mp4, err = ExtractPreviewFMP4(path, 3500*time.Millisecond)
	require.NoError(t, err)
	requirePreviewMarker(t, mp4, 2, 0, 4)

	mp4, err = ExtractPreviewFMP4(path, time.Hour)
	require.NoError(t, err)
	requirePreviewMarker(t, mp4, 4, 0)
}

func TestPreviewUnixFMP4SeeksToTime(t *testing.T) {
	gin.SetMode(gin.TestMode)

	path := filepath.Join(t.TempDir(), "cam1.mp4")
	writeFMP4PreviewParts(t, path, 6, 2)
	start := time.Unix(1786648428, 0).UTC()

	idx := NewIndex()
	idx.Add("cam1", path, start)

	s := &Server{
		PathConfs: map[string]*conf.Path{
			"cam1": {Name: "cam1"},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
		Index:       idx,
	}
	r := gin.New()
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodGet, "/cam1/1786648431-preview.mp4", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "video/mp4", w.Header().Get("Content-Type"))
	requirePreviewMarker(t, w.Body.Bytes(), 2, 0)
}
