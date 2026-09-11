package compatapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recorder"
)

func TestHLSChunkByteRange(t *testing.T) {
	chunks := []hlsMediaChunk{
		{Off: 1000, N: 5, Duration: 5 * time.Second},
		{Off: 50_000, N: 5, Duration: 5 * time.Second},
		{Off: 90_000, N: 6, Duration: 6 * time.Second},
	}
	start, n, ok := hlsChunkByteRange(chunks, 1000, 5, 120_000)
	require.True(t, ok)
	require.Equal(t, int64(1000), start)
	require.Equal(t, int64(49_000), n)

	start, n, ok = hlsChunkByteRange(chunks, 90_000, 6, 120_000)
	require.True(t, ok)
	require.Equal(t, int64(90_000), start)
	require.Equal(t, int64(30_000), n)

	_, _, ok = hlsChunkByteRange(chunks, 1000, 4, 120_000)
	require.False(t, ok)
	_, _, ok = hlsChunkByteRange(nil, 1000, 5, 120_000)
	require.False(t, ok)
}

func TestGenerateArchiveM3U8IndexedUsesStoredChunksWithoutFile(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	chunks := []hlsMediaChunk{
		{Off: 2048, N: 5, Duration: 5 * time.Second, PTSEnd: 5 * time.Second, MoofCount: 5},
		{Off: 40_000, N: 5, Duration: 5 * time.Second, PTSEnd: 10 * time.Second, MoofCount: 5},
		{Off: 80_000, N: 5, Duration: 50 * time.Second, PTSEnd: 60 * time.Second, MoofCount: 5},
	}
	body := GenerateArchiveM3U8Indexed(conf.RecordFormatFMP4, []*IndexedSegment{{
		Rel:   "missing-on-disk.mp4",
		Start: base,
		fmp4: fmp4SegMeta{
			Duration:  time.Minute,
			MoofCount: 60,
			Ready:     true,
			Chunks:    chunks,
		},
	}}, time.Minute, 5*time.Second, 0, time.Time{})

	require.NotContains(t, body, "#EXTINF:60.000,")
	require.Contains(t, body, "off=2048")
	require.Contains(t, body, "off=40000")
	require.Contains(t, body, "&n=5")
	require.GreaterOrEqual(t, strings.Count(body, "hls=media&off="), 3)
}

func TestGenerateArchiveM3U8IndexedNoScanWithoutChunks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hour.mp4")
	writeFMP4Parts(t, path, 20, 2)
	base := time.Unix(1000, 0).UTC()
	body := GenerateArchiveM3U8Indexed(conf.RecordFormatFMP4, []*IndexedSegment{{
		Rel:    filepath.Base(path),
		common: dir,
		Start:  base,
		fmp4:   fmp4SegMeta{Duration: 20 * time.Second, MoofCount: 20, Ready: true},
	}}, 20*time.Second, 5*time.Second, 0, time.Time{})
	require.Contains(t, body, "#EXTINF:20.000,")
	require.NotContains(t, body, "off=")
}

func TestPackOverlayCopiesChunksByStart(t *testing.T) {
	day, dayUnix := testPackDay(t)
	short := day.Add(3 * time.Hour)
	long := short.Add(5 * time.Second)
	src := &dvrPack{hash: testPackHash, dayUnix: dayUnix}
	src.appendSeg(dvrPackSeg{Start: short, Duration: 5 * time.Second, Ready: true})
	src.appendSeg(dvrPackSeg{
		Start: long, Duration: time.Minute, Ready: true,
		Chunks: []dvrPackChunk{
			{Off: 2048, Duration: 6 * time.Second, PTSEnd: 6 * time.Second, Moof: 6},
			{Off: 400_000, Duration: 54 * time.Second, PTSEnd: time.Minute, Moof: 54},
		},
	})

	dir := t.TempDir()
	packPath := filepath.Join(dir, ".mtx-dvr-index.pack")
	require.NoError(t, writeDayPackFile(packPath, src))

	got, err := loadDayPack(packPath, testPackHash)
	require.NoError(t, err)

	segs := []*IndexedSegment{
		{Start: short, fmp4: fmp4SegMeta{Duration: 5 * time.Second, Ready: true}},
		{Start: long, fmp4: fmp4SegMeta{Duration: time.Minute, Ready: true}},
	}
	overlayPackChunks(segs, got)
	require.Empty(t, segs[0].fmp4.Chunks)
	require.Len(t, segs[1].fmp4.Chunks, 2)
	require.Equal(t, int64(2048), segs[1].fmp4.Chunks[0].Off)
	require.Equal(t, 6, segs[1].fmp4.Chunks[0].N)
	require.Equal(t, 6*time.Second, segs[1].fmp4.Chunks[0].Duration)
}

func TestFillChunkJobStoresChunksAndPlaylistSkipsDisk(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "long.mp4")
	writeFMP4Parts(t, fpath, 20, 2)
	base := time.Unix(1_577_836_800, 0).UTC()

	idx := NewIndex()
	idx.pathConfs = map[string]*conf.Path{
		"cam1": {
			Name:                   "cam1",
			RecordFormat:           conf.RecordFormatFMP4,
			RecordHlsChunkDuration: conf.Duration(5 * time.Second),
		},
	}
	idx.Add("cam1", fpath, base)
	idx.SetFMP4Meta("cam1", fpath, fmp4SegMeta{Duration: 20 * time.Second, MoofCount: 20, Ready: true})

	idx.fillChunkJob(chunkJob{pathName: "cam1", fpath: fpath})

	idx.mutex.RLock()
	seg := idx.paths["cam1"].byName[filepath.Base(fpath)]
	require.GreaterOrEqual(t, len(seg.fmp4.Chunks), 2)
	require.Greater(t, seg.fmp4.Size, int64(0))
	chunks := append([]hlsMediaChunk(nil), seg.fmp4.Chunks...)
	idx.mutex.RUnlock()

	require.NoError(t, os.Remove(fpath))
	body := GenerateArchiveM3U8Indexed(conf.RecordFormatFMP4, []*IndexedSegment{{
		Rel:   filepath.Base(fpath),
		Start: base,
		fmp4: fmp4SegMeta{
			Duration:  20 * time.Second,
			MoofCount: 20,
			Ready:     true,
			Chunks:    chunks,
		},
	}}, 20*time.Second, 5*time.Second, 0, time.Time{})
	require.NotContains(t, body, "#EXTINF:20.000,")
	require.Contains(t, body, "off=")
}

func TestHLSSliceRangeInvalidatesOnSizeMismatch(t *testing.T) {
	idx := NewIndex()
	fpath := "/rec/long.mp4"
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", fpath, base)
	idx.SetFMP4Meta("cam1", fpath, fmp4SegMeta{
		Duration: 20 * time.Second,
		Ready:    true,
		Size:     1000,
		Chunks: []hlsMediaChunk{
			{Off: 100, N: 5, Duration: 5 * time.Second},
			{Off: 500, N: 5, Duration: 15 * time.Second},
		},
	})

	_, _, ok := idx.hlsSliceRange("cam1", fpath, 100, 5, 2000)
	require.False(t, ok)
	idx.mutex.RLock()
	require.Empty(t, idx.paths["cam1"].byName["long.mp4"].fmp4.Chunks)
	idx.mutex.RUnlock()
}

func TestCompleteSegmentUsesRecorderPartsWithoutRescan(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	fpath := datedFMP4(t, dir, "cam1", start, 2)
	require.NoError(t, os.Remove(fpath))

	pc := testRecordPathConf(dir, "cam1")
	pc.RecordSegmentDuration = conf.Duration(time.Minute)
	pc.RecordHlsChunkDuration = conf.Duration(5 * time.Second)
	idx := NewIndex()
	idx.EnablePersist(map[string]*conf.Path{"cam1": pc})

	parts := make([]recorder.SegmentPart, 20)
	off := int64(1000)
	for i := range parts {
		parts[i] = recorder.SegmentPart{
			Off:      off,
			Len:      1000,
			Duration: time.Second,
			DTSStart: time.Duration(i) * time.Second,
			HasIDR:   i%2 == 0,
		}
		off += 1000
	}

	idx.CompleteSegment("cam1", fpath, 20*time.Second, parts)

	idx.mutex.RLock()
	seg := idx.paths["cam1"].byName[filepath.Base(fpath)]
	require.GreaterOrEqual(t, len(seg.fmp4.Chunks), 2)
	require.Equal(t, off, seg.fmp4.Size)
	chunks := append([]hlsMediaChunk(nil), seg.fmp4.Chunks...)
	idx.mutex.RUnlock()

	body := GenerateArchiveM3U8Indexed(conf.RecordFormatFMP4, []*IndexedSegment{{
		Rel:   filepath.Base(fpath),
		Start: start,
		fmp4: fmp4SegMeta{
			Duration:  20 * time.Second,
			MoofCount: 20,
			Ready:     true,
			Chunks:    chunks,
		},
	}}, 20*time.Second, 5*time.Second, 0, time.Time{})
	require.NotContains(t, body, "#EXTINF:20.000,")
	require.Contains(t, body, "off=")
}
