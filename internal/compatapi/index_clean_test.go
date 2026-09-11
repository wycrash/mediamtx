package compatapi

import (
	"path/filepath"
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

func TestIndexOldestOnDisk(t *testing.T) {
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
	idx.Add("cam1", filepath.Join(d2, "cam1", "otherdisk.mp4"), t0.Add(-time.Hour)) // older but other disk
	idx.Add("cam1", filepath.Join(d1, "cam1", "new.mp4"), t0.Add(2*time.Hour))

	idx.mutex.Lock()
	idx.paths["cam1"].complete = true
	idx.paths["cam2"].complete = true
	idx.mutex.Unlock()

	refs, ok := idx.OldestOnDisk("dvr", d1, 2)
	require.True(t, ok)
	require.Len(t, refs, 2)
	require.Equal(t, "old.mp4", filepath.Base(refs[0].Fpath))
	require.Equal(t, "mid.mp4", filepath.Base(refs[1].Fpath))

	names, ok := idx.PathNames()
	require.True(t, ok)
	require.Equal(t, []string{"cam1", "cam2"}, names)
}

func TestIndexOldestOnDiskIncompleteFallsBack(t *testing.T) {
	dir := t.TempDir()
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {Name: "cam1", Storage: "dvr", StorageDisks: []string{dir}, Record: true},
	})
	idx.Add("cam1", filepath.Join(dir, "cam1", "a.mp4"), time.Now())
	// incomplete
	_, ok := idx.OldestOnDisk("dvr", dir, 10)
	require.False(t, ok)
}
