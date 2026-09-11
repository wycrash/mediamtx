package compatapi

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/stretchr/testify/require"
)

// writeMeta must flush incremental diskRanges/diskDays only — never re-merge
// every pe.segments entry into an already-complete diskRanges (O(N²) on DVR).
func TestWriteMetaFlushesIncrementalCaches(t *testing.T) {
	dir := t.TempDir()
	pathConf := &conf.Path{
		Name:                  "cam1",
		Record:                true,
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d/%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatMPEGTS,
		RecordSegmentDuration: conf.Duration(time.Hour),
		RecordPartDuration:    conf.Duration(5 * time.Second),
	}
	pathConfs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	idx.mutex.Lock()
	idx.pathConfs = pathConfs
	pe := idx.ensurePathLocked("cam1")
	pe.setLayouts(makeDvrLayouts(pathConf, "cam1"))
	pe.complete = true
	pe.persist = newDvrPersist(pathConf, "cam1")
	pe.persist.ready = true
	common := pe.layout.common
	require.NotEmpty(t, common)

	base := time.Date(2026, 9, 8, 10, 0, 0, 0, time.Local)
	const n = 5000
	for i := 0; i < n; i++ {
		start := base.Add(time.Duration(i) * 5 * time.Second)
		fpath := filepath.Join(common, start.Format("2006-01-02"), start.Format("15-04-05-000000")+".ts")
		seg := bindSeg(pe, fpath, start)
		seg.fmp4 = fmp4SegMeta{Duration: 5 * time.Second, Ready: true}
		seg.countedInMeta = true
		pe.byName[seg.Name()] = seg
		pe.segments = append(pe.segments, seg)
		pe.appendDiskRange(common, start, 5*time.Second)
	}
	pe.setDayNSeg(dvrDayDate(base), n)
	pe.setDiskDayNSeg(common, dvrDayDate(base), n)
	beforeRanges := append([]RecordingRange(nil), pe.diskRanges[common]...)
	require.NotEmpty(t, beforeRanges)
	// A healthy incremental cache is a handful of merged ranges, not one per segment.
	require.Less(t, len(beforeRanges), 20)
	idx.mutex.Unlock()

	t0 := time.Now()
	idx.writeMeta("cam1")
	require.Less(t, time.Since(t0), 500*time.Millisecond, "writeMeta must not re-merge all segments")

	meta, err := readMetaFile(pe.layout.meta)
	require.NoError(t, err)
	require.Equal(t, beforeRanges, meta.Ranges)
	require.Len(t, meta.Days, 1)
	require.Equal(t, uint32(n), meta.Days[0].NSeg)
}
