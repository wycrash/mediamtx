package recordcleaner

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/test"
)

type fakeLister struct {
	paths      []string
	pathsOK    bool
	before     map[string][]SegmentRef
	beforeOK   bool
	oldest     []SegmentRef
	oldestOK   bool
	beforeN    atomic.Int32
	oldestN    atomic.Int32
	pathNamesN atomic.Int32
}

func (f *fakeLister) PathNames() ([]string, bool) {
	f.pathNamesN.Add(1)
	return f.paths, f.pathsOK
}

func (f *fakeLister) SegmentsBefore(pathName string, end time.Time) ([]SegmentRef, bool) {
	f.beforeN.Add(1)
	if !f.beforeOK {
		return nil, false
	}
	var out []SegmentRef
	for _, r := range f.before[pathName] {
		if !r.Start.After(end) {
			out = append(out, r)
		}
	}
	return out, true
}

func (f *fakeLister) ReclaimCandidates(storageName, diskRoot string, limit int) ([]SegmentRef, bool) {
	f.oldestN.Add(1)
	if !f.oldestOK {
		return nil, false
	}
	if limit < len(f.oldest) {
		return f.oldest[:limit], true
	}
	return f.oldest, true
}

func (f *fakeLister) Flush() {}

func TestCleanerAgeUsesSegmentLister(t *testing.T) {
	timeNow = func() time.Time {
		return time.Date(2009, 5, 20, 22, 15, 25, 427000, time.Local)
	}
	t.Cleanup(func() { timeNow = time.Now })

	dir := t.TempDir()
	pathDir := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(pathDir, 0o755))

	// Old enough to expire with deleteAfter=10s from frozen now.
	oldSeg := filepath.Join(pathDir, "2008-05-20_22-15-25-000125.mp4")
	keepSeg := filepath.Join(pathDir, "2009-05-20_22-15-25-000427.mp4")
	require.NoError(t, os.WriteFile(oldSeg, []byte{1}, 0o644))
	require.NoError(t, os.WriteFile(keepSeg, []byte{1}, 0o644))

	// Extra file that WalkDir would see but lister will not list — must survive.
	orphan := filepath.Join(pathDir, "2007-01-01_00-00-00-000000.mp4")
	require.NoError(t, os.WriteFile(orphan, []byte{1}, 0o644))

	lister := &fakeLister{
		paths:    []string{"cam1"},
		pathsOK:  true,
		beforeOK: true,
		before: map[string][]SegmentRef{
			"cam1": {
				{PathName: "cam1", Fpath: oldSeg, Start: time.Date(2008, 5, 20, 22, 15, 25, 125000, time.Local)},
				{PathName: "cam1", Fpath: keepSeg, Start: time.Date(2009, 5, 20, 22, 15, 25, 427000, time.Local)},
			},
		},
	}

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:              "cam1",
				RecordPath:        filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat:      conf.RecordFormatFMP4,
				RecordDeleteAfter: conf.Duration(10 * time.Second),
			},
		},
		Parent: test.NilLogger,
	}
	c.SetSegmentLister(lister)
	c.doRun()

	require.Equal(t, int32(1), lister.pathNamesN.Load())
	require.Equal(t, int32(1), lister.beforeN.Load())
	_, err := os.Stat(oldSeg)
	require.Error(t, err)
	_, err = os.Stat(keepSeg)
	require.NoError(t, err)
	_, err = os.Stat(orphan)
	require.NoError(t, err, "lister path must not WalkDir leftover files")
}

func TestCleanerSpaceUsesSegmentLister(t *testing.T) {
	dir := t.TempDir()
	pathDir := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(pathDir, 0o755))

	oldSeg := filepath.Join(pathDir, "2008-05-20_22-15-25-000125.mp4")
	newSeg := filepath.Join(pathDir, "2009-05-20_22-15-25-000427.mp4")
	require.NoError(t, os.WriteFile(oldSeg, []byte{1}, 0o644))
	require.NoError(t, os.WriteFile(newSeg, []byte{1}, 0o644))

	maxPct := 90.0
	untilPct := 85.0
	var usage atomic.Value
	usage.Store(95.0)

	lister := &fakeLister{
		oldestOK: true,
		oldest: []SegmentRef{
			{PathName: "cam1", Fpath: oldSeg, Start: time.Date(2008, 5, 20, 22, 15, 25, 0, time.UTC)},
			{PathName: "cam1", Fpath: newSeg, Start: time.Date(2009, 5, 20, 22, 15, 25, 0, time.UTC)},
		},
	}

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:         "cam1",
				Storage:      "dvr",
				StorageDisks: []string{dir},
				RecordPath:   "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat: conf.RecordFormatFMP4,
				Record:       true,
			},
		},
		Storages: map[string]*conf.Storage{
			"dvr": {
				MaxUsedPercent:     &maxPct,
				DeleteUntilPercent: &untilPct,
				Disks:              []string{dir},
			},
		},
		Parent:            test.NilLogger,
		UsageRecheckEvery: 1,
		OnSegmentRemove: func(string) {
			usage.Store(80.0)
		},
		DiskUsedPercent: func(string) (float64, error) {
			return usage.Load().(float64), nil
		},
	}
	c.SetSegmentLister(lister)
	c.doRun()

	require.GreaterOrEqual(t, lister.oldestN.Load(), int32(1))
	_, err := os.Stat(oldSeg)
	require.Error(t, err)
	_, err = os.Stat(newSeg)
	require.NoError(t, err)
}

func TestCleanerListerFallback(t *testing.T) {
	timeNow = func() time.Time {
		return time.Date(2009, 5, 20, 22, 15, 25, 427000, time.Local)
	}
	t.Cleanup(func() { timeNow = time.Now })

	dir := t.TempDir()
	pathDir := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(pathDir, 0o755))
	oldSeg := filepath.Join(pathDir, "2008-05-20_22-15-25-000125.mp4")
	require.NoError(t, os.WriteFile(oldSeg, []byte{1}, 0o644))

	lister := &fakeLister{pathsOK: false, beforeOK: false}
	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:              "cam1",
				RecordPath:        filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat:      conf.RecordFormatFMP4,
				RecordDeleteAfter: conf.Duration(10 * time.Second),
			},
		},
		Parent: test.NilLogger,
	}
	c.SetSegmentLister(lister)
	c.doRun()

	_, err := os.Stat(oldSeg)
	require.Error(t, err, "fallback WalkDir must still delete expired")
}

func TestCleanerIdleSkipsReclaimWhenBelowMax(t *testing.T) {
	dir := t.TempDir()
	maxPct := 90.0
	untilPct := 80.0
	lister := &fakeLister{pathsOK: true, paths: []string{"cam1"}, beforeOK: true}
	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:         "cam1",
				Storage:      "dvr",
				StorageDisks: []string{dir},
				RecordPath:   "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat: conf.RecordFormatFMP4,
				Record:       true,
			},
		},
		Storages: map[string]*conf.Storage{
			"dvr": {
				MaxUsedPercent:     &maxPct,
				DeleteUntilPercent: &untilPct,
				Disks:              []string{dir},
			},
		},
		DiskUsage: func(string) (diskUsageInfo, error) {
			return diskUsageInfo{usedPercent: 53.4, totalBytes: 3 << 40}, nil
		},
		Parent: test.NilLogger,
	}
	c.SetSegmentLister(lister)
	interval := c.doRun()
	require.Equal(t, int32(0), lister.oldestN.Load(), "idle must not scan reclaim journals")
	require.Equal(t, int32(0), lister.beforeN.Load())
	require.GreaterOrEqual(t, interval, 30*time.Minute)
}

func TestCleanerIdleIntervalHalfDeleteAfter(t *testing.T) {
	dir := t.TempDir()
	maxPct := 90.0
	untilPct := 80.0
	lister := &fakeLister{pathsOK: true, paths: []string{"cam1"}, beforeOK: true}
	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:              "cam1",
				Storage:           "dvr",
				StorageDisks:      []string{dir},
				RecordPath:        "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat:      conf.RecordFormatFMP4,
				Record:            true,
				RecordDeleteAfter: conf.Duration(20 * time.Minute),
			},
		},
		Storages: map[string]*conf.Storage{
			"dvr": {
				MaxUsedPercent:     &maxPct,
				DeleteUntilPercent: &untilPct,
				Disks:              []string{dir},
			},
		},
		DiskUsage: func(string) (diskUsageInfo, error) {
			return diskUsageInfo{usedPercent: 53.4, totalBytes: 3 << 40}, nil
		},
		Parent: test.NilLogger,
	}
	c.SetSegmentLister(lister)
	interval := c.doRun()
	require.Equal(t, int32(0), lister.oldestN.Load())
	require.Equal(t, 10*time.Minute, interval)
}

func TestCleanerOverMaxReclaimShortInterval(t *testing.T) {
	dir := t.TempDir()
	pathDir := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(pathDir, 0o755))
	seg := filepath.Join(pathDir, "2008-05-20_22-15-25-000125.mp4")
	require.NoError(t, os.WriteFile(seg, []byte{1}, 0o644))

	maxPct := 90.0
	untilPct := 80.0
	var usage atomic.Value
	usage.Store(91.0)
	lister := &fakeLister{
		oldestOK: true,
		oldest: []SegmentRef{
			{PathName: "cam1", Fpath: seg, Start: time.Date(2008, 5, 20, 22, 15, 25, 0, time.UTC)},
		},
	}
	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:         "cam1",
				Storage:      "dvr",
				StorageDisks: []string{dir},
				RecordPath:   "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat: conf.RecordFormatFMP4,
				Record:       true,
			},
		},
		Storages: map[string]*conf.Storage{
			"dvr": {
				MaxUsedPercent:     &maxPct,
				DeleteUntilPercent: &untilPct,
				Disks:              []string{dir},
			},
		},
		Parent:            test.NilLogger,
		UsageRecheckEvery: 1,
		OnSegmentRemove: func(string) {
			usage.Store(85.0)
		},
		DiskUsage: func(string) (diskUsageInfo, error) {
			return diskUsageInfo{usedPercent: usage.Load().(float64), totalBytes: 1 << 40}, nil
		},
	}
	c.SetSegmentLister(lister)
	interval := c.doRun()
	require.GreaterOrEqual(t, lister.oldestN.Load(), int32(1))
	require.Equal(t, spacePressureInterval, interval)
	require.True(t, c.isReclaiming(dir))
}

func TestCleanerKickStartsReclaim(t *testing.T) {
	dir := t.TempDir()
	pathDir := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(pathDir, 0o755))
	seg := filepath.Join(pathDir, "2008-05-20_22-15-25-000125.mp4")
	require.NoError(t, os.WriteFile(seg, []byte{1}, 0o644))

	maxPct := 90.0
	untilPct := 80.0
	var usage atomic.Value
	usage.Store(53.4)
	lister := &fakeLister{
		pathsOK:  true,
		paths:    []string{"cam1"},
		beforeOK: true,
		oldestOK: true,
		oldest: []SegmentRef{
			{PathName: "cam1", Fpath: seg, Start: time.Date(2008, 5, 20, 22, 15, 25, 0, time.UTC)},
		},
	}
	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:         "cam1",
				Storage:      "dvr",
				StorageDisks: []string{dir},
				RecordPath:   "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat: conf.RecordFormatFMP4,
				Record:       true,
			},
		},
		Storages: map[string]*conf.Storage{
			"dvr": {
				MaxUsedPercent:     &maxPct,
				DeleteUntilPercent: &untilPct,
				Disks:              []string{dir},
			},
		},
		Parent:            test.NilLogger,
		UsageRecheckEvery: 1,
		OnSegmentRemove: func(string) {
			usage.Store(79.0)
		},
		DiskUsage: func(string) (diskUsageInfo, error) {
			return diskUsageInfo{usedPercent: usage.Load().(float64), totalBytes: 1 << 40}, nil
		},
	}
	c.SetSegmentLister(lister)
	c.Initialize()
	defer c.Close()

	require.Eventually(t, func() bool {
		return lister.pathNamesN.Load() >= 1
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, int32(0), lister.oldestN.Load())

	usage.Store(91.0)
	c.Kick()
	require.Eventually(t, func() bool {
		return lister.oldestN.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond)
}
