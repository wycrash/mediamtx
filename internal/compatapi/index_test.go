package compatapi

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/stretchr/testify/require"
)

func TestIndexRangesCached(t *testing.T) {
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
	idx.Add("cam1", "/rec/cam1/c.ts", base.Add(60*time.Second))

	r1 := idx.Ranges("cam1")
	require.Equal(t, []RecordingRange{
		{From: 1000, Duration: 20},
		{From: 1060, Duration: 10},
	}, r1)

	// cached copy
	r2 := idx.Ranges("cam1")
	require.Equal(t, r1, r2)

	idx.Add("cam1", "/rec/cam1/d.ts", base.Add(70*time.Second))
	r3 := idx.Ranges("cam1")
	require.Equal(t, []RecordingRange{
		{From: 1000, Duration: 20},
		{From: 1060, Duration: 20},
	}, r3)

	idx.Remove("/rec/cam1/c.ts")
	idx.Remove("/rec/cam1/d.ts")
	r4 := idx.Ranges("cam1")
	require.Equal(t, []RecordingRange{
		{From: 1000, Duration: 20},
	}, r4)
}

func TestIndexFindByName(t *testing.T) {
	idx := NewIndex()
	idx.Add("cam1", filepath.Join("rec", "cam1", "seg.ts"), time.Unix(1, 0))
	fpath, ok := idx.FindByName("cam1", "seg.ts")
	require.True(t, ok)
	require.Equal(t, filepath.Join("rec", "cam1", "seg.ts"), fpath)
}

func TestIndexSegmentsInWindow(t *testing.T) {
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {
			Name:                  "cam1",
			RecordSegmentDuration: conf.Duration(10 * time.Second),
		},
	})
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", "/a.ts", base)
	idx.Add("cam1", "/b.ts", base.Add(5*time.Second))
	idx.Add("cam1", "/c.ts", base.Add(20*time.Second))

	out := idx.SegmentsInWindow("cam1", base, 10*time.Second)
	require.Len(t, out, 2)
	require.Equal(t, "/a.ts", out[0].Fpath)
	require.Equal(t, "/b.ts", out[1].Fpath)
}

func TestIndexSegmentsInWindowIncludesOverlap(t *testing.T) {
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {
			Name:                  "cam1",
			RecordSegmentDuration: conf.Duration(time.Hour),
		},
	})
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", "/a.mp4", base)
	idx.SetFMP4Meta("cam1", "/a.mp4", fmp4SegMeta{Duration: time.Hour, Ready: true})
	idx.Add("cam1", "/b.mp4", base.Add(time.Hour))
	idx.SetFMP4Meta("cam1", "/b.mp4", fmp4SegMeta{Duration: time.Hour, Ready: true})

	// Seek 10 minutes into the first hour-long file.
	out := idx.SegmentsInWindow("cam1", base.Add(10*time.Minute), time.Hour)
	require.Len(t, out, 2)
	require.Equal(t, "/a.mp4", out[0].Fpath)
	require.Equal(t, "/b.mp4", out[1].Fpath)
}

func TestIndexSetFMP4Meta(t *testing.T) {
	idx := NewIndex()
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", "/rec/a.mp4", base)
	idx.SetFMP4Meta("cam1", "/rec/a.mp4", fmp4SegMeta{
		Duration:  10 * time.Second,
		MoofCount: 4,
		Ready:     true,
	})

	out := idx.SegmentsInWindow("cam1", base, time.Minute)
	require.Len(t, out, 1)
	require.True(t, out[0].fmp4.Ready)
	require.Equal(t, 10*time.Second, out[0].fmp4.Duration)
	require.Equal(t, uint32(4), out[0].fmp4.MoofCount)
	require.Equal(t, 1, idx.SegmentCount())
}

func TestIndexRangesUsesFMP4DurationAndKeepsHoles(t *testing.T) {
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {
			Name:                  "cam1",
			RecordSegmentDuration: conf.Duration(time.Hour),
		},
	})
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", "/rec/a.mp4", base)
	idx.Add("cam1", "/rec/b.mp4", base.Add(60*time.Second))
	idx.SetFMP4Meta("cam1", "/rec/a.mp4", fmp4SegMeta{Duration: 10 * time.Second, Ready: true})
	idx.SetFMP4Meta("cam1", "/rec/b.mp4", fmp4SegMeta{Duration: 10 * time.Second, Ready: true})

	require.Equal(t, []RecordingRange{
		{From: 1000, Duration: 10},
		{From: 1060, Duration: 10},
	}, idx.Ranges("cam1"))
}

func TestIndexRangesOpenSegmentGrowsToNow(t *testing.T) {
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {
			Name:                  "cam1",
			RecordSegmentDuration: conf.Duration(time.Hour),
		},
	})
	start := time.Now().Add(-5 * time.Second)
	idx.Add("cam1", "/rec/open.mp4", start)
	r := idx.Ranges("cam1")
	require.Len(t, r, 1)
	require.InDelta(t, 5, r[0].Duration, 2)
}
