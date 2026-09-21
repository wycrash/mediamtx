package compatapi

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
)

func TestIndexSegmentsBefore(t *testing.T) {
	dir := t.TempDir()
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {Name: "cam1", Storage: "dvr", StorageDisks: []string{dir}},
	})

	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	idx.Add("cam1", filepath.Join(dir, "cam1", "a.mp4"), base)
	idx.Add("cam1", filepath.Join(dir, "cam1", "b.mp4"), base.Add(time.Hour))
	idx.Add("cam1", filepath.Join(dir, "cam1", "c.mp4"), base.Add(2*time.Hour))

	_, ok := idx.SegmentsBefore("cam1", base.Add(time.Hour))
	require.False(t, ok, "incomplete path must fall back")

	idx.mutex.Lock()
	idx.paths["cam1"].complete = true
	idx.mutex.Unlock()

	refs, ok := idx.SegmentsBefore("cam1", base.Add(time.Hour))
	require.True(t, ok)
	require.Len(t, refs, 2)
	require.Equal(t, "a.mp4", filepath.Base(refs[0].Fpath))
	require.Equal(t, "b.mp4", filepath.Base(refs[1].Fpath))

	refs, ok = idx.SegmentsBefore("cam1", base.Add(-time.Second))
	require.True(t, ok)
	require.Empty(t, refs)
}

func TestIndexReclaimCandidatesRAMComplete(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {Name: "cam1", Storage: "dvr", StorageDisks: []string{d1, d2}, Record: true},
		"cam2": {Name: "cam2", Storage: "dvr", StorageDisks: []string{d1, d2}, Record: true},
	})

	t0 := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	idx.Add("cam1", filepath.Join(d1, "cam1", "old.mp4"), t0)
	idx.Add("cam2", filepath.Join(d1, "cam2", "mid.mp4"), t0.Add(time.Minute))
	idx.Add("cam1", filepath.Join(d2, "cam1", "otherdisk.mp4"), t0.Add(-time.Hour))
	idx.Add("cam1", filepath.Join(d1, "cam1", "new.mp4"), t0.Add(2*time.Hour))

	idx.mutex.Lock()
	idx.paths["cam1"].complete = true
	idx.paths["cam2"].complete = true
	idx.mutex.Unlock()

	refs, ok := idx.ReclaimCandidates("dvr", d1, 2)
	require.True(t, ok)
	require.GreaterOrEqual(t, len(refs), 3)
	require.Equal(t, "otherdisk.mp4", filepath.Base(refs[0].Fpath), "older file on B is a companion")
	require.Equal(t, "old.mp4", filepath.Base(refs[1].Fpath))
	require.Equal(t, "mid.mp4", filepath.Base(refs[2].Fpath))

	names, ok := idx.PathNames()
	require.True(t, ok)
	require.Equal(t, []string{"cam1", "cam2"}, names)
}

func TestIndexReclaimCandidatesIncompleteNoRAM(t *testing.T) {
	dir := t.TempDir()
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {Name: "cam1", Storage: "dvr", StorageDisks: []string{dir}, Record: true},
	})
	idx.Add("cam1", filepath.Join(dir, "cam1", "a.mp4"), time.Now())
	refs, ok := idx.ReclaimCandidates("dvr", dir, 10)
	require.True(t, ok, "incomplete path must not WalkDir; listing/empty is ok")
	require.Empty(t, refs, "live RAM must not be treated as oldest archive")
}

func TestIndexReclaimCandidatesNewEngineSkipsToday(t *testing.T) {
	dir := t.TempDir()
	oldStart := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	today := time.Now().Truncate(time.Second)
	oldPath := datedFMP4(t, dir, "cam1", oldStart, 2)
	todayPath := datedFMP4(t, dir, "cam1", today, 2)

	pathConf := testRecordPathConf(dir, "cam1")
	pathConf.Storage = "dvr"
	pathConf.StorageDisks = []string{dir}
	pathConf.Record = true
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	idx = NewIndex()
	idx.ConfigureEngine(conf.IndexEngineNew, 1)
	require.Equal(t, 1, idx.LoadFromDisk(confs).DiskPaths)

	idx.mutex.RLock()
	pe := idx.paths["cam1"]
	require.NotNil(t, pe)
	require.False(t, pe.dayIsPinned(dvrDayDate(oldStart)))
	idx.mutex.RUnlock()

	refs, ok := idx.ReclaimCandidates("dvr", dir, 1)
	require.True(t, ok)
	require.NotEmpty(t, refs)
	require.Equal(t, oldPath, refs[0].Fpath)
	for _, r := range refs {
		require.NotEqual(t, todayPath, r.Fpath)
	}

	before := idx.Ranges("cam1")
	require.NotEmpty(t, before)
	require.LessOrEqual(t, before[0].From, oldStart.Unix())

	idx.RemoveIndexed("cam1", oldPath)
	idx.Flush()

	_, err := os.Stat(oldPath)
	require.NoError(t, err, "RemoveIndexed does not delete the media file")
	require.NoError(t, os.Remove(oldPath))

	after := idx.Ranges("cam1")
	require.NotEmpty(t, after)
	require.Greater(t, after[0].From, oldStart.Unix())
	idx.mutex.RLock()
	require.Equal(t, 0, idx.paths["cam1"].dayNSeg(dvrDayDate(oldStart)))
	require.Greater(t, idx.paths["cam1"].dayNSeg(dvrDayDate(today)), 0)
	idx.mutex.RUnlock()
	idx.ClosePersist()
}

func TestIndexReclaimCandidatesRoundRobinCompanions(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	t0 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	t1 := t0.Add(5 * time.Second)
	t2 := t0.Add(10 * time.Second)
	f0 := datedFMP4(t, d1, "cam1", t0, 2)
	f1 := datedFMP4(t, d2, "cam1", t1, 2)
	f2 := datedFMP4(t, d1, "cam1", t2, 2)

	pathConf := &conf.Path{
		Name:                  "cam1",
		Enabled:               true,
		Storage:               "dvr",
		StorageDisks:          []string{d1, d2},
		RecordPath:            "%path/%Y-%m-%d_%H-%M-%S-%f",
		RecordFormat:          conf.RecordFormatFMP4,
		Record:                true,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
		RecordPartDuration:    conf.Duration(time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 3, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	idx = NewIndex()
	idx.ConfigureEngine(conf.IndexEngineNew, 1)
	require.Equal(t, 1, idx.LoadFromDisk(confs).DiskPaths)

	refs, ok := idx.ReclaimCandidates("dvr", d1, 2)
	require.True(t, ok)
	require.Equal(t, []string{f0, f1, f2}, []string{refs[0].Fpath, refs[1].Fpath, refs[2].Fpath})

	idx.RemoveIndexed("cam1", f0)
	idx.Flush()
	ranges := idx.Ranges("cam1")
	require.NotEmpty(t, ranges)
	require.GreaterOrEqual(t, ranges[0].From, t1.Unix())
	idx.mutex.RLock()
	pe := idx.paths["cam1"]
	require.Equal(t, 1, pe.diskDayNSeg(pe.commonFor(f0), dvrDayDate(t0)), "t2 remains on the same disk/day")
	require.Greater(t, pe.diskDayNSeg(pe.commonFor(f1), dvrDayDate(t1)), 0)
	idx.mutex.RUnlock()
	idx.ClosePersist()
}

func TestIndexReclaimCandidatesOldestCameraFirst(t *testing.T) {
	dir := t.TempDir()
	older := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	newer := time.Date(2024, 1, 1, 0, 0, 0, 0, time.Local)
	cam1 := datedFMP4(t, dir, "cam1", older, 2)
	cam2 := datedFMP4(t, dir, "cam2", newer, 2)

	confs := map[string]*conf.Path{
		"cam1": storagePathConf(dir, "cam1"),
		"cam2": storagePathConf(dir, "cam2"),
	}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	idx = NewIndex()
	idx.ConfigureEngine(conf.IndexEngineNew, 1)
	require.Equal(t, 2, idx.LoadFromDisk(confs).DiskPaths)

	cam1Journal := 0
	cam2Journal := 0
	orig := readFile
	readFile = func(name string) ([]byte, error) {
		if strings.HasSuffix(name, dvrJournalSuffix) {
			if strings.Contains(name, "2020-01-01") {
				cam1Journal++
			}
			if strings.Contains(name, "2024-01-01") {
				cam2Journal++
			}
		}
		return orig(name)
	}
	t.Cleanup(func() { readFile = orig })

	refs, ok := idx.ReclaimCandidates("dvr", dir, 1)
	require.True(t, ok)
	require.NotEmpty(t, refs)
	require.Equal(t, cam1, refs[0].Fpath)
	require.NotEqual(t, cam2, refs[0].Fpath)
	require.Greater(t, cam1Journal, 0)
	require.Equal(t, 0, cam2Journal, "newer camera journal must not be opened")
	idx.ClosePersist()
}

func TestIndexSegmentsBeforeColdJournal(t *testing.T) {
	dir := t.TempDir()
	oldStart := time.Date(2020, 1, 1, 12, 0, 0, 0, time.Local)
	today := time.Now().Truncate(time.Second)
	oldPath := datedFMP4(t, dir, "cam1", oldStart, 2)
	datedFMP4(t, dir, "cam1", today, 2)

	pathConf := storagePathConf(dir, "cam1")
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	idx = NewIndex()
	idx.ConfigureEngine(conf.IndexEngineNew, 1)
	require.Equal(t, 1, idx.LoadFromDisk(confs).DiskPaths)

	refs, ok := idx.SegmentsBefore("cam1", oldStart.Add(time.Minute))
	require.True(t, ok)
	require.Len(t, refs, 1)
	require.Equal(t, oldPath, refs[0].Fpath)
	idx.ClosePersist()
}

func TestIndexListDirCachedOnce(t *testing.T) {
	dir := t.TempDir()
	day := "2020-01-01"
	dayDir := filepath.Join(dir, "cam1", day)
	require.NoError(t, os.MkdirAll(dayDir, 0o755))
	fpath := filepath.Join(dayDir, "2020-01-01_00-00-00-000000.mp4")
	require.NoError(t, os.WriteFile(fpath, []byte{1}, 0o644))

	pathConf := &conf.Path{
		Name:         "cam1",
		Enabled:      true,
		Storage:      "dvr",
		StorageDisks: []string{dir},
		RecordPath:   "%path/%Y-%m-%d/%Y-%m-%d_%H-%M-%S-%f",
		RecordFormat: conf.RecordFormatFMP4,
		Record:       true,
	}
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{"cam1": pathConf})
	idx.mutex.Lock()
	pe := idx.ensurePathLocked("cam1")
	pe.complete = true
	pe.setLayouts(makeDvrLayouts(pathConf, "cam1"))
	pe.days = []dvrDayInfo{{Date: day, NSeg: 1}}
	idx.mutex.Unlock()

	var n atomic.Int32
	orig := readDir
	readDir = func(name string) ([]os.DirEntry, error) {
		n.Add(1)
		return orig(name)
	}
	t.Cleanup(func() { readDir = orig })

	refs, ok := idx.ReclaimCandidates("dvr", dir, 1)
	require.True(t, ok)
	require.NotEmpty(t, refs)
	require.Equal(t, fpath, refs[0].Fpath)
	first := n.Load()
	require.Greater(t, first, int32(0))

	refs, ok = idx.ReclaimCandidates("dvr", dir, 1)
	require.True(t, ok)
	require.NotEmpty(t, refs)
	require.Equal(t, first, n.Load(), "second reclaim must reuse the folder listing cache")
}

func storagePathConf(dir, name string) *conf.Path {
	p := testRecordPathConf(dir, name)
	p.Storage = "dvr"
	p.StorageDisks = []string{dir}
	p.Record = true
	return p
}

func TestIndexSegmentsBeforeIdleOldestDayAfterCutoff(t *testing.T) {
	dir := t.TempDir()
	pathConf := storagePathConf(dir, "cam1")
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{"cam1": pathConf})
	idx.mutex.Lock()
	pe := idx.ensurePathLocked("cam1")
	pe.complete = true
	pe.setLayouts(makeDvrLayouts(pathConf, "cam1"))
	pe.days = []dvrDayInfo{{Date: "2026-09-18", NSeg: 10}}
	idx.mutex.Unlock()

	n := 0
	orig := readFile
	readFile = func(name string) ([]byte, error) {
		n++
		return orig(name)
	}
	t.Cleanup(func() { readFile = orig })

	refs, ok := idx.SegmentsBefore("cam1", time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local))
	require.True(t, ok)
	require.Empty(t, refs)
	require.Equal(t, 0, n)
}

func TestIndexSegmentsBeforeIntraDayOneJournal(t *testing.T) {
	dir := t.TempDir()
	dayStart := time.Date(2020, 1, 1, 10, 0, 0, 0, time.Local)
	oldPath := datedFMP4(t, dir, "cam1", dayStart, 2)
	keepPath := datedFMP4(t, dir, "cam1", dayStart.Add(40*time.Minute), 2)
	nextDayPath := datedFMP4(t, dir, "cam1", dayStart.Add(24*time.Hour), 2)

	pathConf := storagePathConf(dir, "cam1")
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 3, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	idx = NewIndex()
	idx.ConfigureEngine(conf.IndexEngineNew, 1)
	require.Equal(t, 1, idx.LoadFromDisk(confs).DiskPaths)

	journals := map[string]int{}
	orig := readFile
	readFile = func(name string) ([]byte, error) {
		if strings.HasSuffix(name, dvrJournalSuffix) {
			journals[name]++
		}
		return orig(name)
	}
	t.Cleanup(func() { readFile = orig })

	refs, ok := idx.SegmentsBefore("cam1", dayStart.Add(20*time.Minute))
	require.True(t, ok)
	require.Len(t, refs, 1)
	require.Equal(t, oldPath, refs[0].Fpath)
	require.Len(t, journals, 1, "only the overlapping oldest day journal")
	for _, r := range refs {
		require.NotEqual(t, keepPath, r.Fpath)
		require.NotEqual(t, nextDayPath, r.Fpath)
	}

	idx.mutex.RLock()
	days := append([]dvrDayInfo(nil), idx.paths["cam1"].days...)
	idx.mutex.RUnlock()
	require.GreaterOrEqual(t, len(days), 2)

	cutoff1d := dayStart.Add(24 * time.Hour).Add(12 * time.Hour)
	refs, ok = idx.SegmentsBefore("cam1", cutoff1d)
	require.True(t, ok)
	require.NotEmpty(t, refs)
	require.Equal(t, oldPath, refs[0].Fpath)
	for _, r := range refs {
		require.NotEqual(t, nextDayPath, r.Fpath)
	}
	require.Len(t, journals, 1, "remaining days wait for later ticks")
	idx.ClosePersist()
}

func TestIndexReclaimKeepsDayCacheAcrossDeletes(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	f0 := datedFMP4(t, dir, "cam1", t0, 2)
	f1 := datedFMP4(t, dir, "cam1", t0.Add(5*time.Second), 2)
	f2 := datedFMP4(t, dir, "cam1", t0.Add(10*time.Second), 2)

	pathConf := storagePathConf(dir, "cam1")
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 3, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	idx = NewIndex()
	idx.ConfigureEngine(conf.IndexEngineNew, 1)
	require.Equal(t, 1, idx.LoadFromDisk(confs).DiskPaths)

	journalReads := 0
	orig := readFile
	readFile = func(name string) ([]byte, error) {
		if strings.HasSuffix(name, dvrJournalSuffix) && strings.Contains(name, "2020-01-01") {
			journalReads++
		}
		return orig(name)
	}
	t.Cleanup(func() { readFile = orig })

	refs, ok := idx.ReclaimCandidates("dvr", dir, 1)
	require.True(t, ok)
	require.Equal(t, []string{f0}, []string{refs[0].Fpath})
	require.Greater(t, journalReads, 0)
	afterLoad := journalReads

	idx.RemoveIndexed("cam1", f0)
	refs, ok = idx.ReclaimCandidates("dvr", dir, 1)
	require.True(t, ok)
	require.Equal(t, f1, refs[0].Fpath)
	require.Equal(t, afterLoad, journalReads, "same day must not be re-read after a delete")

	idx.RemoveIndexed("cam1", f1)
	refs, ok = idx.ReclaimCandidates("dvr", dir, 1)
	require.True(t, ok)
	require.Equal(t, f2, refs[0].Fpath)
	require.Equal(t, afterLoad, journalReads)
	idx.ClosePersist()
}
