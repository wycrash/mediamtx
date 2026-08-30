package compatapi

import (
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/stretchr/testify/require"
)

func TestIndexMemStats(t *testing.T) {
	idx := NewIndex()
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", "/recordings/cam1/a.mp4", base)
	idx.Add("cam1", "/recordings/cam1/b.mp4", base.Add(5*time.Second))
	idx.Add("cam2", "/recordings/cam2/c.mp4", base)

	tracks := []*fmp4.InitTrack{{
		ID:        1,
		TimeScale: 90000,
		Codec:     &mcodecs.H264{SPS: []byte{1, 2, 3, 4}, PPS: []byte{5, 6}},
	}}
	idx.SetFMP4Meta("cam1", "/recordings/cam1/a.mp4", fmp4SegMeta{
		Duration:  5 * time.Second,
		MoofCount: 5,
		Ready:     true,
	}, tracks)
	idx.SetFMP4Meta("cam1", "/recordings/cam1/b.mp4", fmp4SegMeta{
		Duration:  5 * time.Second,
		MoofCount: 5,
		Ready:     true,
	}, tracks)

	st := idx.MemStats()
	require.Equal(t, 2, st.Paths)
	require.Equal(t, 3, st.Segments)
	require.Equal(t, 2, st.FMP4Ready)
	require.Equal(t, 2, st.SegsWithTracks)
	require.Equal(t, 2, st.TrackPtrs)
	require.Equal(t, 1, st.UniqueTrackPtrs)
	require.Equal(t, 6, st.CodecPayloadBytes)
	require.Equal(t, len("/recordings/cam1/a.mp4")+len("/recordings/cam1/b.mp4")+len("/recordings/cam2/c.mp4"), st.FpathBytes)
	require.Greater(t, st.EstLiveBytes, int64(0))
	require.Len(t, st.PathsDetail, 2)
	require.Equal(t, "cam1", st.PathsDetail[0].Name)
	require.Equal(t, 2, st.PathsDetail[0].Segments)
	require.Equal(t, 2, st.PathsDetail[0].WithTracks)
}

func TestIndexInternsCompatibleTracks(t *testing.T) {
	idx := NewIndex()
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", "/rec/a.mp4", base)
	idx.Add("cam1", "/rec/b.mp4", base.Add(5*time.Second))
	idx.Add("cam1", "/rec/c.mp4", base.Add(10*time.Second))

	h264 := func() *mcodecs.H264 {
		return &mcodecs.H264{SPS: []byte{0x67, 0x64, 0x00, 0x1e}, PPS: []byte{0x68, 0xee}}
	}
	idx.SetFMP4Meta("cam1", "/rec/a.mp4", fmp4SegMeta{
		Duration: time.Second, Ready: true,
	}, []*fmp4.InitTrack{{ID: 1, TimeScale: 90000, Codec: h264()}})
	idx.SetFMP4Meta("cam1", "/rec/b.mp4", fmp4SegMeta{
		Duration: time.Second, Ready: true,
	}, []*fmp4.InitTrack{{ID: 1, TimeScale: 90000, Codec: h264()}})
	idx.SetFMP4Meta("cam1", "/rec/c.mp4", fmp4SegMeta{
		Duration: time.Second, Ready: true,
	}, []*fmp4.InitTrack{{ID: 1, TimeScale: 90000, Codec: &mcodecs.H264{
		SPS: []byte{0x67, 0x64, 0x00, 0x1f}, PPS: []byte{0x68, 0xee},
	}}})

	segs := idx.SegmentsInWindow("cam1", base, time.Minute)
	require.Len(t, segs, 3)
	require.Same(t, segs[0].tracks()[0], segs[1].tracks()[0])
	require.NotSame(t, segs[0].tracks()[0], segs[2].tracks()[0])

	st := idx.MemStats()
	require.Equal(t, 2, st.UniqueTrackPtrs)
	require.Equal(t, 2, st.InternedSets)
	require.Equal(t, 3, st.TrackPtrs)
}

func TestFormatBytes(t *testing.T) {
	require.Equal(t, "512B", formatBytes(512))
	require.Equal(t, "1.0KB", formatBytes(1024))
	require.Equal(t, "640.0MB", formatBytes(640*1024*1024))
}
