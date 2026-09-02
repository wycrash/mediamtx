package storage

import (
	"os"
	"path/filepath"
	"testing"

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

	a, err := r.Pick("dvr", nil)
	require.NoError(t, err)
	b, err := r.Pick("dvr", nil)
	require.NoError(t, err)
	c, err := r.Pick("dvr", nil)
	require.NoError(t, err)
	require.Equal(t, "/a", a)
	require.Equal(t, "/b", b)
	require.Equal(t, "/a", c)
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

	got, err := r.Pick("dvr", nil)
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

	got, err := r.Pick("dvr", nil)
	require.NoError(t, err)
	require.Equal(t, "/a", got)
	got, err = r.Pick("dvr", nil)
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

	got, err := r.Pick("dvr", []string{"/a"})
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

	_, err := r.Pick("dvr", nil)
	require.ErrorIs(t, err, ErrNoWritableDisk)
}

func TestPickMissing(t *testing.T) {
	r := NewRegistry(nil)
	_, err := r.Pick("nope", nil)
	require.ErrorIs(t, err, ErrNotFound)
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
