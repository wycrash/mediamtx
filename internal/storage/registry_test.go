package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
)

func pct(v float64) *float64 { return &v }

func TestPickRoundRobin(t *testing.T) {
	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyRoundRobin,
			MaxUsedPercent: pct(0),
			Disks:          []string{"/a", "/b"},
		},
	})
	r.usage = func(string) (float64, error) { return 0, nil }

	a, err := r.Pick("dvr", "", nil)
	require.NoError(t, err)
	b, err := r.Pick("dvr", "", nil)
	require.NoError(t, err)
	c, err := r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, "/a", a)
	require.Equal(t, "/b", b)
	require.Equal(t, "/a", c)
}

func TestPickRoundRobinPerPath(t *testing.T) {
	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyRoundRobin,
			MaxUsedPercent: pct(0),
			Disks:          []string{"/a", "/b"},
		},
	})
	r.usage = func(string) (float64, error) { return 0, nil }

	// Shared pool cursor used to pin cam1→/a and cam2→/b forever when
	// cameras rolled segments in lockstep. Each path must alternate itself.
	var cam1, cam2 []string
	for i := 0; i < 4; i++ {
		d, err := r.Pick("dvr", "cam1", nil)
		require.NoError(t, err)
		cam1 = append(cam1, d)
		d, err = r.Pick("dvr", "cam2", nil)
		require.NoError(t, err)
		cam2 = append(cam2, d)
	}
	require.Equal(t, cam1[0], cam1[2])
	require.Equal(t, cam1[1], cam1[3])
	require.NotEqual(t, cam1[0], cam1[1])
	require.Equal(t, cam2[0], cam2[2])
	require.Equal(t, cam2[1], cam2[3])
	require.NotEqual(t, cam2[0], cam2[1])
}

func TestPickSkipsFull(t *testing.T) {
	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyRoundRobin,
			MaxUsedPercent: pct(90),
			Disks:          []string{"/full", "/ok"},
		},
	})
	r.usage = func(path string) (float64, error) {
		if path == "/full" {
			return 95, nil
		}
		return 10, nil
	}

	got, err := r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, "/ok", got)
}

func TestPickFillFirst(t *testing.T) {
	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyFillFirst,
			MaxUsedPercent: pct(90),
			Disks:          []string{"/a", "/b"},
		},
	})
	r.usage = func(string) (float64, error) { return 10, nil }

	got, err := r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, "/a", got)
	got, err = r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, "/a", got)
}

func TestPickSkipList(t *testing.T) {
	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyRoundRobin,
			MaxUsedPercent: pct(0),
			Disks:          []string{"/a", "/b"},
		},
	})
	r.usage = func(string) (float64, error) { return 0, nil }

	got, err := r.Pick("dvr", "", []string{"/a"})
	require.NoError(t, err)
	require.Equal(t, "/b", got)
}

func TestPickAllFull(t *testing.T) {
	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyRoundRobin,
			MaxUsedPercent: pct(90),
			Disks:          []string{"/a", "/b"},
		},
	})
	r.usage = func(string) (float64, error) { return 99, nil }

	_, err := r.Pick("dvr", "", nil)
	require.ErrorIs(t, err, ErrNoWritableDisk)
}

func TestPickReclaimDoesNotRefuse(t *testing.T) {
	var pressure atomic.Int32
	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:           conf.StorageStrategyFillFirst,
			MaxUsedPercent:     pct(72),
			DeleteUntilPercent: pct(65),
			Disks:              []string{"/a"},
		},
	})
	r.usage = func(string) (float64, error) { return 72.6, nil }
	r.OnPressure = func() { pressure.Add(1) }

	got, err := r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, "/a", got)
	require.Equal(t, int32(1), pressure.Load())
}

func TestPickMissing(t *testing.T) {
	r := NewRegistry(nil)
	_, err := r.Pick("nope", "", nil)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPickUsageCached(t *testing.T) {
	var calls atomic.Int32
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyFillFirst,
			MaxUsedPercent: pct(90),
			Disks:          []string{"/a"},
		},
	})
	r.now = func() time.Time { return now }
	r.usageTTL = time.Second
	r.usage = func(string) (float64, error) {
		calls.Add(1)
		return 10, nil
	}

	_, err := r.Pick("dvr", "", nil)
	require.NoError(t, err)
	_, err = r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())

	now = now.Add(2 * time.Second)
	_, err = r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, int32(2), calls.Load())
}

func TestPickFullUsageShortTTL(t *testing.T) {
	var calls atomic.Int32
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyFillFirst,
			MaxUsedPercent: pct(90),
			Disks:          []string{"/full", "/ok"},
		},
	})
	r.now = func() time.Time { return now }
	r.usageTTL = 5 * time.Second
	r.usageFullTTL = 200 * time.Millisecond
	r.usage = func(path string) (float64, error) {
		calls.Add(1)
		if path == "/full" {
			return 95, nil
		}
		return 10, nil
	}

	got, err := r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, "/ok", got)
	n := calls.Load()
	require.GreaterOrEqual(t, n, int32(2)) // probed /full and /ok

	_, err = r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, n, calls.Load()) // both cached

	now = now.Add(300 * time.Millisecond)
	_, err = r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Greater(t, calls.Load(), n) // /full rechecked after short TTL
}

func TestInvalidateUsage(t *testing.T) {
	var calls atomic.Int32
	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyFillFirst,
			MaxUsedPercent: pct(90),
			Disks:          []string{"/a"},
		},
	})
	r.usage = func(string) (float64, error) {
		calls.Add(1)
		return 10, nil
	}

	_, err := r.Pick("dvr", "", nil)
	require.NoError(t, err)
	r.InvalidateUsage()
	_, err = r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, int32(2), calls.Load())
}

func TestPickUsageSingleflight(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})

	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyFillFirst,
			MaxUsedPercent: pct(90),
			Disks:          []string{"/a"},
		},
	})
	r.usage = func(string) (float64, error) {
		calls.Add(1)
		close(started)
		<-release
		return 10, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Pick("dvr", "", nil)
			require.NoError(t, err)
		}()
	}
	<-started
	close(release)
	wg.Wait()
	require.Equal(t, int32(1), calls.Load())
}

func TestRoots(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {Disks: []string{dir, filepath.Join(dir, "b")}},
	})
	require.Equal(t, []string{dir, filepath.Join(dir, "b")}, r.Roots("dvr"))
	require.Empty(t, r.Roots(""))
	_ = os.MkdirAll(filepath.Join(dir, "b"), 0o755)
}

func TestMarkFailedBackoff(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyFillFirst,
			MaxUsedPercent: pct(0),
			Disks:          []string{"/dead", "/ok"},
		},
	})
	r.now = func() time.Time { return now }
	r.firstUnavailableTTL = 3 * time.Minute
	r.repeatUnavailableTTL = 10 * time.Minute
	r.usage = func(string) (float64, error) { return 0, nil }

	ttl := r.MarkFailed("/dead", fmt.Errorf("A device which does not exist was specified."))
	require.Equal(t, 3*time.Minute, ttl)
	reason, bad := r.Unusable("/dead")
	require.True(t, bad)
	require.Contains(t, reason, "device which does not exist")
	reason, bad = r.Unusable("/dead/cam1")
	require.True(t, bad)
	got, err := r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, "/ok", got)

	// Parallel failures while cooling down must not jump to 10 minutes.
	ttl = r.MarkFailed("/dead", nil)
	require.Equal(t, 3*time.Minute, ttl)

	now = now.Add(3 * time.Minute)
	got, err = r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, "/dead", got)

	ttl = r.MarkFailed("/dead", nil)
	require.Equal(t, 10*time.Minute, ttl)
	got, err = r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, "/ok", got)

	now = now.Add(9 * time.Minute)
	got, err = r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, "/ok", got)

	now = now.Add(time.Minute)
	got, err = r.Pick("dvr", "", nil)
	require.NoError(t, err)
	require.Equal(t, "/dead", got)

	r.MarkOK("/dead")
	ttl = r.MarkFailed("/dead", nil)
	require.Equal(t, 3*time.Minute, ttl)
}

func TestPickAllUnavailable(t *testing.T) {
	r := NewRegistry(map[string]*conf.Storage{
		"dvr": {
			Strategy:       conf.StorageStrategyRoundRobin,
			MaxUsedPercent: pct(0),
			Disks:          []string{"/a", "/b"},
		},
	})
	r.usage = func(string) (float64, error) { return 0, nil }
	r.MarkFailed("/a", nil)
	r.MarkFailed("/b", nil)
	_, err := r.Pick("dvr", "", nil)
	require.ErrorIs(t, err, ErrNoWritableDisk)
}
