package compatapi

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/bluenviron/mediamtx/internal/test"
)

func writeArchiveTestSegment(t *testing.T, fpath string) {
	t.Helper()

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
		},
	}

	var buf1 seekablebuffer.Buffer
	err := init.Marshal(&buf1)
	require.NoError(t, err)

	var buf2 seekablebuffer.Buffer
	parts := fmp4.Parts{
		{
			Tracks: []*fmp4.PartTrack{
				{
					ID:       1,
					BaseTime: 0,
					Samples: []*fmp4.Sample{
						{Duration: 1 * 90000, Payload: []byte{1, 2}},
						{Duration: 1 * 90000, Payload: []byte{3, 4}},
						{Duration: 1 * 90000, Payload: []byte{5, 6}},
					},
				},
			},
		},
	}
	err = parts.Marshal(&buf2)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Dir(fpath), 0o755))
	require.NoError(t, os.WriteFile(fpath, append(buf1.Bytes(), buf2.Bytes()...), 0o644))
}

func newArchiveTestServer(t *testing.T, dir string) *httptest.Server {
	t.Helper()

	s := &Server{
		PathConfs: map[string]*conf.Path{
			"mypath": {
				Name:         "mypath",
				RecordPath:   filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(s.onRequest)
	return httptest.NewServer(r)
}

func TestArchiveMP4(t *testing.T) {
	dir, err := os.MkdirTemp("", "mtx-compat-archive")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	segStart := time.Date(2008, 11, 7, 11, 22, 0, 0, time.Local)
	pathConf := &conf.Path{
		Name:         "mypath",
		RecordPath:   filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat: conf.RecordFormatFMP4,
	}
	recPath := recordstore.PathAddExtension(
		strings.ReplaceAll(pathConf.RecordPath, "%path", "mypath"),
		pathConf.RecordFormat,
	)
	fpath := recordstore.Path{Path: "mypath", Start: segStart}.Encode(recPath)
	writeArchiveTestSegment(t, fpath)

	segments, err := recordstore.FindSegments(pathConf, "mypath", &segStart, ptrTime(segStart.Add(3*time.Second)))
	require.NoError(t, err)
	require.NotEmpty(t, segments)

	ts := newArchiveTestServer(t, dir)
	defer ts.Close()

	u := fmt.Sprintf("%s/mypath/archive-%d-3.mp4", ts.URL, segStart.Unix())
	res, err := http.Get(u)
	require.NoError(t, err)
	defer res.Body.Close()

	buf, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "video/mp4", res.Header.Get("Content-Type"))
	require.Contains(t, res.Header.Get("Content-Disposition"), "archive-")
	require.Contains(t, res.Header.Get("Content-Disposition"), ".mp4")

	var p pmp4.Presentation
	err = p.Unmarshal(bytes.NewReader(buf))
	require.NoError(t, err)
	require.NotEmpty(t, p.Tracks)
}

func ptrTime(t time.Time) *time.Time {
	return &t
}

func TestArchiveMP4NotFound(t *testing.T) {
	dir, err := os.MkdirTemp("", "mtx-compat-archive-empty")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "mypath"), 0o755))

	ts := newArchiveTestServer(t, dir)
	defer ts.Close()

	res, err := http.Get(ts.URL + "/mypath/archive-1000-10.mp4")
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestArchiveMP4InvalidDuration(t *testing.T) {
	dir, err := os.MkdirTemp("", "mtx-compat-archive-dur")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	ts := newArchiveTestServer(t, dir)
	defer ts.Close()

	res, err := http.Get(ts.URL + "/mypath/archive-1000-0.mp4")
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusBadRequest, res.StatusCode)
}
