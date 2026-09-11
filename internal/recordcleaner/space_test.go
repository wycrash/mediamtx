package recordcleaner

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/test"
)

func TestCleanerDiskPressure(t *testing.T) {
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

	var removed []string
	var mu sync.Mutex

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:         "cam1",
				Storage:      "dvr",
				StorageDisks: []string{dir},
				RecordPath:   "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		Storages: map[string]*conf.Storage{
			"dvr": {
				Strategy:           conf.StorageStrategyRoundRobin,
				MaxUsedPercent:     &maxPct,
				DeleteUntilPercent: &untilPct,
				Disks:              []string{dir},
			},
		},
		Parent:            test.NilLogger,
		UsageRecheckEvery: 1,
		OnSegmentRemove: func(fpath string) {
			mu.Lock()
			removed = append(removed, fpath)
			mu.Unlock()
			usage.Store(80.0)
		},
		DiskUsedPercent: func(string) (float64, error) {
			return usage.Load().(float64), nil
		},
	}

	c.doRun()

	_, err := os.Stat(oldSeg)
	require.Error(t, err)
	_, err = os.Stat(newSeg)
	require.NoError(t, err)

	mu.Lock()
	require.Len(t, removed, 1)
	require.Equal(t, oldSeg, removed[0])
	mu.Unlock()
}

func TestCleanerDiskPressureStopsAtDeleteUntil(t *testing.T) {
	dir := t.TempDir()
	pathDir := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(pathDir, 0o755))

	seg1 := filepath.Join(pathDir, "2008-05-20_22-15-25-000001.mp4")
	seg2 := filepath.Join(pathDir, "2008-05-20_22-15-25-000002.mp4")
	seg3 := filepath.Join(pathDir, "2008-05-20_22-15-25-000003.mp4")
	for _, s := range []string{seg1, seg2, seg3} {
		require.NoError(t, os.WriteFile(s, []byte{1}, 0o644))
	}

	maxPct := 90.0
	untilPct := 85.0
	var n atomic.Int32
	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:              "cam1",
				Storage:           "dvr",
				StorageDisks:      []string{dir},
				RecordPath:        "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat:      conf.RecordFormatFMP4,
				RecordDeleteAfter: 0,
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
		DiskUsedPercent: func(string) (float64, error) {
			// First probe: over max. After first delete: still over until. After second: done.
			switch n.Add(1) {
			case 1:
				return 95, nil
			case 2:
				return 88, nil
			default:
				return 84, nil
			}
		},
	}

	c.doRun()

	_, err := os.Stat(seg1)
	require.Error(t, err)
	_, err = os.Stat(seg2)
	require.Error(t, err)
	_, err = os.Stat(seg3)
	require.NoError(t, err)
}

func TestCleanerSkipsActiveSegment(t *testing.T) {
	dir := t.TempDir()
	pathDir := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(pathDir, 0o755))

	only := filepath.Join(pathDir, "2008-05-20_22-15-25-000125.mp4")
	require.NoError(t, os.WriteFile(only, []byte{1}, 0o644))

	maxPct := 90.0
	untilPct := 85.0
	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:         "cam1",
				Storage:      "dvr",
				StorageDisks: []string{dir},
				RecordPath:   "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		Storages: map[string]*conf.Storage{
			"dvr": {
				MaxUsedPercent:     &maxPct,
				DeleteUntilPercent: &untilPct,
				Disks:              []string{dir},
			},
		},
		Parent:          test.NilLogger,
		IsActiveSegment: func(fpath string) bool { return fpath == only },
		DiskUsedPercent: func(string) (float64, error) { return 95, nil },
	}

	c.doRun()

	_, err := os.Stat(only)
	require.NoError(t, err)
}

func TestCleanerSpaceBeforeAge(t *testing.T) {
	timeNow = func() time.Time {
		return time.Date(2009, 5, 20, 22, 15, 25, 427000, time.Local)
	}
	t.Cleanup(func() { timeNow = time.Now })

	dir := t.TempDir()
	pathDir := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(pathDir, 0o755))

	expired := filepath.Join(pathDir, "2008-05-20_22-15-25-000125.mp4")
	keep := filepath.Join(pathDir, "2009-05-20_22-15-25-000427.mp4")
	require.NoError(t, os.WriteFile(expired, []byte{1}, 0o644))
	require.NoError(t, os.WriteFile(keep, []byte{1}, 0o644))

	maxPct := 90.0
	untilPct := 85.0
	var order []string
	var mu sync.Mutex

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:              "cam1",
				Storage:           "dvr",
				StorageDisks:      []string{dir},
				RecordPath:        "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat:      conf.RecordFormatFMP4,
				RecordDeleteAfter: conf.Duration(10 * time.Second),
			},
		},
		Storages: map[string]*conf.Storage{
			"dvr": {
				MaxUsedPercent:     &maxPct,
				DeleteUntilPercent: &untilPct,
				Disks:              []string{dir},
			},
		},
		Parent: test.NilLogger,
		OnSegmentRemove: func(fpath string) {
			mu.Lock()
			if fpath == expired {
				order = append(order, "age")
			} else {
				order = append(order, "space:"+filepath.Base(fpath))
			}
			mu.Unlock()
		},
		DiskUsedPercent: func(string) (float64, error) {
			mu.Lock()
			order = append(order, "space-check")
			mu.Unlock()
			return 50, nil // under max — space must not delete
		},
	}

	c.doRun()

	_, err := os.Stat(expired)
	require.Error(t, err)
	_, err = os.Stat(keep)
	require.NoError(t, err)

	mu.Lock()
	// Space reclaim runs before age retention when deleteUntilPercent is set.
	require.Equal(t, []string{"space-check", "age"}, order)
	mu.Unlock()
}

func TestCleanerUsageRecheckBatched(t *testing.T) {
	dir := t.TempDir()
	pathDir := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(pathDir, 0o755))

	segs := []string{
		filepath.Join(pathDir, "2008-05-20_22-15-25-000001.mp4"),
		filepath.Join(pathDir, "2008-05-20_22-15-25-000002.mp4"),
		filepath.Join(pathDir, "2008-05-20_22-15-25-000003.mp4"),
		filepath.Join(pathDir, "2008-05-20_22-15-25-000004.mp4"),
		filepath.Join(pathDir, "2008-05-20_22-15-25-000005.mp4"),
	}
	for _, s := range segs {
		require.NoError(t, os.WriteFile(s, []byte{1}, 0o644))
	}

	maxPct := 90.0
	untilPct := 85.0
	var probes atomic.Int32
	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:         "cam1",
				Storage:      "dvr",
				StorageDisks: []string{dir},
				RecordPath:   "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat: conf.RecordFormatFMP4,
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
		UsageRecheckEvery: 3,
		DiskUsedPercent: func(string) (float64, error) {
			n := probes.Add(1)
			if n == 1 {
				return 95, nil
			}
			return 80, nil // after first recheck batch
		},
	}

	c.doRun()

	// initial (>=max) + recheck after 3 deletes (<=until) + final "now %" probe
	require.Equal(t, int32(3), probes.Load())
	for i, s := range segs {
		_, err := os.Stat(s)
		if i < 3 {
			require.Error(t, err, s)
		} else {
			require.NoError(t, err, s)
		}
	}
}

func TestCleanerOnSpaceFreedOnce(t *testing.T) {
	dir := t.TempDir()
	pathDir := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(pathDir, 0o755))
	oldSeg := filepath.Join(pathDir, "2008-05-20_22-15-25-000125.mp4")
	require.NoError(t, os.WriteFile(oldSeg, []byte{1}, 0o644))

	maxPct := 90.0
	untilPct := 85.0
	var usage atomic.Value
	usage.Store(95.0)
	var freed atomic.Int32
	var removes atomic.Int32

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:         "cam1",
				Storage:      "dvr",
				StorageDisks: []string{dir},
				RecordPath:   "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat: conf.RecordFormatFMP4,
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
			removes.Add(1)
			usage.Store(80.0)
		},
		OnSpaceFreed: func() { freed.Add(1) },
		DiskUsedPercent: func(string) (float64, error) {
			return usage.Load().(float64), nil
		},
	}
	c.doRun()
	require.Equal(t, int32(1), removes.Load())
	require.Equal(t, int32(1), freed.Load())
}

func TestCleanerReclaimContinuesAcrossPasses(t *testing.T) {
	dir := t.TempDir()
	pathDir := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(pathDir, 0o755))

	seg1 := filepath.Join(pathDir, "2008-05-20_22-15-25-000001.mp4")
	seg2 := filepath.Join(pathDir, "2008-05-20_22-15-25-000002.mp4")
	require.NoError(t, os.WriteFile(seg1, []byte{1}, 0o644))

	maxPct := 72.0
	untilPct := 65.0
	var usage atomic.Value
	usage.Store(73.0)

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:               "cam1",
				Storage:            "dvr",
				StorageDisks:       []string{dir},
				RecordPath:         "%path/%Y-%m-%d_%H-%M-%S-%f",
				RecordFormat:       conf.RecordFormatFMP4,
				RecordMaxPartSize:  50 * 1024 * 1024,
				RecordPartDuration: conf.Duration(time.Hour),
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
			usage.Store(71.0) // below max, still above until
		},
		DiskUsage: func(string) (diskUsageInfo, error) {
			return diskUsageInfo{usedPercent: usage.Load().(float64), totalBytes: 1 << 40}, nil
		},
	}

	// Pass 1: hit max, delete only available segment → 71%, stay reclaiming.
	interval := c.doRun()
	require.Equal(t, spacePressureInterval, interval)
	require.True(t, c.isReclaiming(dir))
	_, err := os.Stat(seg1)
	require.Error(t, err)

	// Idle hysteresis would skip at 71% without reclaiming flag — must continue.
	require.NoError(t, os.WriteFile(seg2, []byte{1}, 0o644))
	c.OnSegmentRemove = func(string) {
		usage.Store(64.0)
	}
	interval = c.doRun()
	_, err = os.Stat(seg2)
	require.Error(t, err)
	require.False(t, c.isReclaiming(dir))
	require.NotEqual(t, spacePressureInterval, interval)

	// Between until and max with reclaim cleared: do not delete.
	require.NoError(t, os.WriteFile(seg1, []byte{1}, 0o644))
	usage.Store(70.0)
	interval = c.doRun()
	require.NotEqual(t, spacePressureInterval, interval)
	_, err = os.Stat(seg1)
	require.NoError(t, err)
}

func TestCleanerMaintMuSerializes(t *testing.T) {
	var mu sync.Mutex
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32

	enter := func() {
		cur := concurrent.Add(1)
		for {
			prev := maxConcurrent.Load()
			if cur <= prev || maxConcurrent.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		concurrent.Add(-1)
	}

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{},
		Parent:    test.NilLogger,
		MaintMu:   &mu,
	}

	done := make(chan struct{})
	go func() {
		mu.Lock()
		enter()
		mu.Unlock()
		close(done)
	}()

	time.Sleep(5 * time.Millisecond)
	c.doRun() // waits on MaintMu, then runs empty clean
	<-done

	require.Equal(t, int32(1), maxConcurrent.Load())
}
