package compatapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

func writeH264LPCMSegment(t *testing.T, fpath string) {
	t.Helper()
	f, err := os.Create(fpath)
	require.NoError(t, err)
	defer f.Close()

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{
				ID:        1,
				TimeScale: 90000,
				Codec: &mcodecs.H264{
					SPS: test.FormatH264.SPS,
					PPS: test.FormatH264.PPS,
				},
			},
			{
				ID:        2,
				TimeScale: 8000,
				Codec: &mcodecs.LPCM{
					BitDepth:     16,
					SampleRate:   8000,
					ChannelCount: 1,
				},
			},
		},
	}
	require.NoError(t, init.Marshal(f))
	require.NoError(t, (fmp4.Part{
		SequenceNumber: 0,
		Tracks: []*fmp4.PartTrack{
			{
				ID:       2,
				BaseTime: 0,
				Samples:  []*fmp4.Sample{{Duration: 160, Payload: bytes.Repeat([]byte{0}, 320)}},
			},
			{
				ID:       1,
				BaseTime: 0,
				Samples:  []*fmp4.Sample{{Duration: 90000, Payload: []byte{0, 0, 0, 1, 9, 0xf0}}},
			},
		},
	}).Marshal(f))
}

func serveHLSPart(t *testing.T, fpath, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/cam1/seg.mp4?"+rawQuery, nil)
	require.NoError(t, serveFMP4ArchivePart(c, fpath))
	return w
}

func TestHLSPartsStripLPCM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seg.mp4")
	writeH264LPCMSegment(t, path)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Contains(raw, []byte("ipcm")))

	initW := serveHLSPart(t, path, "hls=init")
	require.Equal(t, http.StatusOK, initW.Code)
	initBody := initW.Body.Bytes()
	require.False(t, bytes.Contains(initBody, []byte("ipcm")))

	var init fmp4.Init
	require.NoError(t, init.Unmarshal(bytes.NewReader(initBody)))
	require.Len(t, init.Tracks, 1)
	require.Equal(t, 1, init.Tracks[0].ID)
	_, ok := init.Tracks[0].Codec.(*mcodecs.H264)
	require.True(t, ok)

	mediaW := serveHLSPart(t, path, "hls=media&sn=0&td=0")
	require.Equal(t, http.StatusOK, mediaW.Code)
	var parts fmp4.Parts
	require.NoError(t, parts.Unmarshal(mediaW.Body.Bytes()))
	require.Len(t, parts, 1)
	require.Len(t, parts[0].Tracks, 1)
	require.Equal(t, 1, parts[0].Tracks[0].ID)

	fullW := serveHLSPart(t, path, "")
	require.Equal(t, http.StatusOK, fullW.Code)
	require.True(t, bytes.Contains(fullW.Body.Bytes(), []byte("ipcm")))
}

func TestHLSPartsKeepAAC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aac.mp4")
	f, err := os.Create(path)
	require.NoError(t, err)

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{
				ID:        1,
				TimeScale: 90000,
				Codec: &mcodecs.H264{
					SPS: test.FormatH264.SPS,
					PPS: test.FormatH264.PPS,
				},
			},
			{
				ID:        2,
				TimeScale: 44100,
				Codec: &mcodecs.MPEG4Audio{
					Config: *test.FormatMPEG4Audio.Config,
				},
			},
		},
	}
	require.NoError(t, init.Marshal(f))
	require.NoError(t, (fmp4.Part{
		SequenceNumber: 0,
		Tracks: []*fmp4.PartTrack{
			{ID: 1, Samples: []*fmp4.Sample{{Duration: 90000, Payload: []byte{0, 0, 0, 1, 9, 0xf0}}}},
			{ID: 2, Samples: []*fmp4.Sample{{Duration: 1024, Payload: []byte{1, 2, 3, 4}}}},
		},
	}).Marshal(f))
	require.NoError(t, f.Close())

	initW := serveHLSPart(t, path, "hls=init")
	require.Equal(t, http.StatusOK, initW.Code)
	var parsed fmp4.Init
	require.NoError(t, parsed.Unmarshal(bytes.NewReader(initW.Body.Bytes())))
	require.Len(t, parsed.Tracks, 2)

	mediaW := serveHLSPart(t, path, "hls=media&sn=0&td=0")
	require.Equal(t, http.StatusOK, mediaW.Code)
	var parts fmp4.Parts
	require.NoError(t, parts.Unmarshal(mediaW.Body.Bytes()))
	require.Len(t, parts[0].Tracks, 2)
}

func TestRewriteFMP4MediaForHLSAppliesTimeline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seg.mp4")
	writeH264LPCMSegment(t, path)

	mediaW := serveHLSPart(t, path, "hls=media&sn=7&td=1000")
	require.Equal(t, http.StatusOK, mediaW.Code)
	var parts fmp4.Parts
	require.NoError(t, parts.Unmarshal(mediaW.Body.Bytes()))
	require.Equal(t, uint32(7), parts[0].SequenceNumber)
	require.Equal(t, 1, parts[0].Tracks[0].ID)
	require.Equal(t, uint64(90000), parts[0].Tracks[0].BaseTime)
}
