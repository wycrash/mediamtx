package compatapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/formatlabel"
	"github.com/bluenviron/mediamtx/internal/test"
)

type testPathAPI struct {
	path *defs.APIPath
	err  error
}

func (m *testPathAPI) APIPathsGet(string) (*defs.APIPath, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.path, nil
}

func TestBuildInfoLiveTracksAndDVR(t *testing.T) {
	tracks := defs.MediasToTracks([]*description.Media{
		test.MediaH264,
		test.MediaMPEG4Audio,
	})
	path := &defs.APIPath{
		Name:      "cam1",
		Available: true,
		Online:    true,
		Tracks2:   tracks,
	}
	ranges := []RecordingRange{
		{From: 1000, Duration: 20},
		{From: 1060, Duration: 10},
	}

	out := buildInfo("cam1", path, ranges, nil)
	require.Equal(t, "cam1", out.Name)
	require.Equal(t, "running", out.Stats.Status)
	require.Equal(t, "stream", out.Stats.MediaInfo.FlowType)
	require.Equal(t, 0, out.Stats.MediaInfo.StreamID)
	require.Equal(t, []InfoTrack{
		{
			Profile: "Baseline",
			Level:   "4",
			Width:   1920,
			Height:  1080,
			Codec:   "h264",
			TrackID: "v1",
			Content: "video",
		},
		{
			Codec:      "aac",
			TrackID:    "a1",
			Content:    "audio",
			Channels:   2,
			SampleRate: 44100,
		},
	}, out.Stats.MediaInfo.Tracks)
	require.Equal(t, InfoDVR{
		Depth:    70,
		Duration: 30,
		From:     1000,
		Ranges:   ranges,
	}, out.Stats.DVRInfo)

	raw, err := json.Marshal(out)
	require.NoError(t, err)
	var generic map[string]any
	require.NoError(t, json.Unmarshal(raw, &generic))
	stats := generic["stats"].(map[string]any)
	_, hasToken := generic["auth_token"]
	require.False(t, hasToken)
	_, hasDTS := stats["last_dts_at"]
	require.False(t, hasDTS)
	dvr := stats["dvr_info"].(map[string]any)
	_, hasDisk := dvr["disk_size"]
	require.False(t, hasDisk)
	video := stats["media_info"].(map[string]any)["tracks"].([]any)[0].(map[string]any)
	_, hasBitrate := video["bitrate"]
	require.False(t, hasBitrate)
	_, hasFPS := video["fps"]
	require.False(t, hasFPS)
}

func TestBuildInfoG711AndWaiting(t *testing.T) {
	path := &defs.APIPath{
		Name:      "cam1",
		Available: false,
		Tracks2: []defs.APIPathTrack{{
			Codec: formatlabel.G711,
			CodecProps: &defs.APIPathTrackCodecPropsG711{
				MULaw:        false,
				SampleRate:   8000,
				ChannelCount: 1,
			},
		}},
	}
	out := buildInfo("cam1", path, nil, nil)
	require.Equal(t, "waiting", out.Stats.Status)
	require.Equal(t, []InfoTrack{{
		Codec:      "pcma",
		TrackID:    "a1",
		Content:    "audio",
		Channels:   1,
		SampleRate: 8000,
	}}, out.Stats.MediaInfo.Tracks)
	require.Empty(t, out.Stats.DVRInfo.Ranges)
}

func TestBuildInfoFMP4Fallback(t *testing.T) {
	rec := []*fmp4.InitTrack{{
		ID:        1,
		TimeScale: 90000,
		Codec: &mcodecs.H264{
			SPS: test.FormatH264.SPS,
			PPS: test.FormatH264.PPS,
		},
	}}
	out := buildInfo("cam1", nil, nil, rec)
	require.Equal(t, "waiting", out.Stats.Status)
	require.Equal(t, "h264", out.Stats.MediaInfo.Tracks[0].Codec)
	require.Equal(t, 1920, out.Stats.MediaInfo.Tracks[0].Width)
	require.Equal(t, "v1", out.Stats.MediaInfo.Tracks[0].TrackID)
}

func TestInfoJSONHTTP(t *testing.T) {
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {
			Name:                  "cam1",
			RecordSegmentDuration: conf.Duration(10 * time.Second),
		},
	})
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", "/rec/cam1/a.ts", base)
	idx.Add("cam1", "/rec/cam1/b.ts", base.Add(10*time.Second))

	s := &Server{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:                  "cam1",
				RecordSegmentDuration: conf.Duration(10 * time.Second),
			},
		},
		PathManager: &testPathAPI{
			path: &defs.APIPath{
				Name:      "cam1",
				Available: true,
				Tracks2: defs.MediasToTracks([]*description.Media{
					test.MediaH264,
				}),
			},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
		Index:       idx,
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(s.onRequest)
	ts := httptest.NewServer(r)
	defer ts.Close()

	res, err := http.Get(ts.URL + "/cam1/info.json")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	var out InfoJSON
	require.NoError(t, json.NewDecoder(res.Body).Decode(&out))
	require.Equal(t, "cam1", out.Name)
	require.Equal(t, "running", out.Stats.Status)
	require.Equal(t, "h264", out.Stats.MediaInfo.Tracks[0].Codec)
	require.Equal(t, int64(1000), out.Stats.DVRInfo.From)
	require.Equal(t, int64(20), out.Stats.DVRInfo.Duration)
	require.Equal(t, int64(20), out.Stats.DVRInfo.Depth)
}

func TestIndexLatestFMP4Tracks(t *testing.T) {
	idx := NewIndex()
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", "/rec/a.mp4", base)
	idx.Add("cam1", "/rec/b.mp4", base.Add(10*time.Second))
	tracks := []*fmp4.InitTrack{{
		ID: 1,
		Codec: &mcodecs.H264{
			SPS: test.FormatH264.SPS,
			PPS: test.FormatH264.PPS,
		},
	}}
	idx.SetFMP4Meta("cam1", "/rec/b.mp4", fmp4SegMeta{
		Ready: true,
	}, tracks)
	got := idx.LatestFMP4Tracks("cam1")
	require.Equal(t, tracks, got)
	require.Nil(t, idx.LatestFMP4Tracks("missing"))
}
