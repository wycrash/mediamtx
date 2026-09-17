package compatapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/bluenviron/mediamtx/internal/test"
)

// Segments are cut on keyframes, so a real file name carries arbitrary
// microseconds in %f. The edge scan has to find it by listing the directory:
// stepping timestamps by segment duration, as the old probe did, produces a
// name that no file ever has.
func TestReconcileAdoptsOffGridSegment(t *testing.T) {
	for _, ca := range []struct {
		name     string
		template string
	}{
		{"flat", "%path/%Y-%m-%d_%H-%M-%S-%f"},
		{"dateDir", "%path/%Y-%m-%d/%H-%M-%S-%f"},
	} {
		t.Run(ca.name, func(t *testing.T) {
			dir := t.TempDir()
			pathConf := &conf.Path{
				Name:                  "cam1",
				RecordPath:            filepath.Join(dir, ca.template),
				RecordFormat:          conf.RecordFormatFMP4,
				RecordSegmentDuration: conf.Duration(5 * time.Second),
				RecordPartDuration:    conf.Duration(time.Second),
			}
			format := recordstore.PathAddExtension(
				strings.ReplaceAll(pathConf.RecordPath, "%path", "cam1"),
				pathConf.RecordFormat,
			)
			write := func(at time.Time) string {
				fpath := recordstore.Path{Start: at}.Encode(format)
				writeNamedFMP4(t, fpath, 2)
				return filepath.Base(fpath)
			}

			base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
			write(base)
			write(base.Add(5 * time.Second))

			idx := NewIndex()
			confs := map[string]*conf.Path{"cam1": pathConf}
			idx.LoadFromDisk(confs)
			require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)

			odd := write(base.Add(10*time.Second + 123456*time.Microsecond))
			// The 24 h edge window straddles midnight, so the scan must cover
			// the next day too: its own directory, or its own name prefix.
			nextDay := write(base.Add(24*time.Hour + 2*time.Second + 7*time.Microsecond))
			idx.ReconcileAll(nil, false)

			// Two queries: SegmentsInWindow clamps to maxArchiveDuration, so
			// one window cannot span both the first minute and past midnight.
			found := map[string]bool{}
			for _, w := range [][2]time.Time{
				{base, base.Add(time.Minute)},
				{base.Add(23 * time.Hour), base.Add(25 * time.Hour)},
			} {
				for _, s := range idx.SegmentsInWindow("cam1", w[0], w[1].Sub(w[0])) {
					found[s.Name()] = true
				}
			}
			require.True(t, found[odd], "off-grid segment must be adopted by the directory scan")
			require.True(t, found[nextDay], "segment past midnight must be adopted by the directory scan")
			idx.ClosePersist()
		})
	}
}

func TestMarkNeedsRebuildForcesDiskRescan(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(cam, 0o755))
	a := filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(cam, "2020-01-01_00-00-05-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 2)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).Segments)
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)
	require.Equal(t, 0, idx.NeedsRebuildCount())

	idx.MarkNeedsRebuild("cam1")
	require.Equal(t, 1, idx.NeedsRebuildCount())
	st := idx.ReconcileAll(nil, false)
	require.Equal(t, 1, st.Built)
	require.Equal(t, 2, st.Segments)
	require.Equal(t, 0, idx.NeedsRebuildCount())
	idx.ClosePersist()
}

func TestAPIIndexRebuildCoalescesPaths(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"cam1", "cam2", "cam3"} {
		cam := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(cam, 0o755))
		writeNamedFMP4(t, filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4"), 2)
	}

	pathConfs := map[string]*conf.Path{}
	for _, name := range []string{"cam1", "cam2", "cam3"} {
		pathConfs[name] = &conf.Path{
			Name:                  name,
			RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
			RecordFormat:          conf.RecordFormatFMP4,
			RecordSegmentDuration: conf.Duration(5 * time.Second),
		}
	}

	s := &Server{
		Address:      "127.0.0.1:0",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		PathConfs:    pathConfs,
		AuthManager:  test.NilAuthManager,
		Parent:       test.NilLogger,
	}
	require.NoError(t, s.Initialize())
	defer s.Close()

	require.Eventually(t, func() bool {
		st, err := s.APIIndexStatus()
		return err == nil && st.State == defs.APICompatIndexStatusIdle
	}, 5*time.Second, 20*time.Millisecond)

	out1, err := s.APIIndexRebuild("cam1")
	require.NoError(t, err)
	require.Equal(t, defs.APIOKStatusOK, out1.Status)
	require.Equal(t, "cam1", out1.Path)

	out2, err := s.APIIndexRebuild("cam1")
	require.NoError(t, err)
	require.Equal(t, out1.Queued, out2.Queued, "duplicate cam1 must not grow the queue")

	out3, err := s.APIIndexRebuild("cam2")
	require.NoError(t, err)
	require.GreaterOrEqual(t, out3.Queued, 2)

	outAll, err := s.APIIndexRebuild("")
	require.NoError(t, err)
	require.True(t, outAll.All)
	require.GreaterOrEqual(t, outAll.Queued, 3)

	_, err = s.APIIndexRebuild("no-such-cam")
	require.ErrorIs(t, err, ErrPathNotFound)
	require.EqualError(t, err, "path 'no-such-cam' not found")

	st, err := s.APIIndexStatus()
	require.NoError(t, err)
	require.Contains(t, []defs.APICompatIndexStatusState{
		defs.APICompatIndexStatusIdle,
		defs.APICompatIndexStatusRebuild,
		defs.APICompatIndexStatusUpdate,
	}, st.State)
	require.NotNil(t, st.Queue)

	// Worker must finish without leaving permanent incomplete marks.
	require.Eventually(t, func() bool {
		return s.Index.NeedsRebuildCount() == 0
	}, 5*time.Second, 20*time.Millisecond)

	st, err = s.APIIndexStatus()
	require.NoError(t, err)
	require.Equal(t, defs.APICompatIndexStatusIdle, st.State)
	require.Empty(t, st.Current)
	require.Empty(t, st.Queue)
	require.Equal(t, 0, st.Queued)
}

func TestAPIIndexRebuildDoesNotScanOtherPaths(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"cam1", "cam2"} {
		cam := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(cam, 0o755))
		writeNamedFMP4(t, filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4"), 2)
	}

	pathConfs := map[string]*conf.Path{}
	for _, name := range []string{"cam1", "cam2"} {
		pathConfs[name] = &conf.Path{
			Name:                  name,
			RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
			RecordFormat:          conf.RecordFormatFMP4,
			RecordSegmentDuration: conf.Duration(5 * time.Second),
		}
	}

	s := &Server{
		Address:      "127.0.0.1:0",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		PathConfs:    pathConfs,
		AuthManager:  test.NilAuthManager,
		Parent:       test.NilLogger,
	}
	require.NoError(t, s.Initialize())
	defer s.Close()

	require.Eventually(t, func() bool {
		st, err := s.APIIndexStatus()
		if err != nil || st.State != defs.APICompatIndexStatusIdle {
			return false
		}
		return s.Index.NeedsRebuildCount() == 0 && s.Index.SegmentCount() >= 2
	}, 5*time.Second, 20*time.Millisecond)

	var mu sync.Mutex
	var logs []string
	s.Parent = test.Logger(func(_ logger.Level, format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	})

	_, err := s.APIIndexRebuild("cam1")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return s.Index.NeedsRebuildCount() == 0
	}, 5*time.Second, 20*time.Millisecond)

	mu.Lock()
	got := append([]string(nil), logs...)
	mu.Unlock()
	joined := strings.Join(got, "\n")
	require.NotContains(t, joined, "updating path=cam2")
	require.Contains(t, joined, "updating path=cam1")
}

// Live CompleteSegment during rebuild marks the day pinned with only the live
// edge. pinDay must still merge the day snapshot or archive m3u8 stays empty.
func TestPinDayMergesWhenAlreadyPinned(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(cam, 0o755))
	a := filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(cam, "2020-01-01_00-00-05-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 2)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).Segments)
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)

	day := "2020-01-01"
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)

	// Simulate live bindPersist during rebuild: day pinned, only the newest seg in RAM.
	idx.mutex.Lock()
	pe := idx.paths["cam1"]
	require.NotNil(t, pe)
	var latest *IndexedSegment
	for _, s := range pe.segments {
		if latest == nil || s.Start.After(latest.Start) {
			latest = s
		}
	}
	require.NotNil(t, latest)
	pe.segments = []*IndexedSegment{latest}
	pe.byName = map[string]*IndexedSegment{latest.Name(): latest}
	pe.pinnedDays = map[string]struct{}{day: {}}
	idx.mutex.Unlock()

	require.Len(t, pe.segments, 1, "precondition: only live edge in RAM")

	idx.pinDay("cam1", day)
	idx.mutex.RLock()
	nRAM := len(idx.paths["cam1"].segments)
	idx.mutex.RUnlock()
	require.Equal(t, 2, nRAM, "pinDay must merge day snapshot into an already-pinned day")
	out := idx.SegmentsInWindow("cam1", start, time.Minute)

	body := GenerateArchiveM3U8Indexed(conf.RecordFormatFMP4, out, 5*time.Second, 0, 0, start)
	require.Contains(t, body, filepath.Base(a))
	require.Contains(t, body, filepath.Base(b))
	require.NotContains(t, body, "#EXT-X-ENDLIST\n#EXTM3U") // not only header+end
	idx.ClosePersist()
}

func TestIndexPathDisableEnableKeepsArchive(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(cam, 0o755))
	a := filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(cam, "2020-01-01_00-00-05-000000.mp4")
	c := filepath.Join(cam, "2020-01-01_00-00-10-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 2)
	writeNamedFMP4WithTrack(t, c, 2, testH264TrackAlt())

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
		RecordPartDuration:    conf.Duration(time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).Segments)
	idx.CompleteSegment("cam1", a, 5*time.Second, nil)
	idx.CompleteSegment("cam1", b, 5*time.Second, nil)
	require.Equal(t, 1, idx.MemStats().UniqueTrackPtrs)

	day := "2020-01-01"
	idx.pinDay("cam1", day)
	idx.mutex.Lock()
	pe := idx.paths["cam1"]
	require.NotNil(t, pe)
	var latest *IndexedSegment
	for _, s := range pe.segments {
		if latest == nil || s.Start.After(latest.Start) {
			latest = s
		}
	}
	require.NotNil(t, latest)
	pe.segments = []*IndexedSegment{latest}
	pe.byName = map[string]*IndexedSegment{latest.Name(): latest}
	pe.pinnedDays = map[string]struct{}{day: {}}
	idx.mutex.Unlock()

	idx.OnPathDisabled("cam1")
	idx.mutex.RLock()
	require.False(t, idx.paths["cam1"].persist.ready)
	idx.mutex.RUnlock()

	idx.OnPathEnabled("cam1")
	idx.mutex.RLock()
	nRAM := len(idx.paths["cam1"].segments)
	idx.mutex.RUnlock()
	require.Equal(t, 2, nRAM, "re-enable must merge the pinned day, not leave only the live edge")

	idx.CompleteSegment("cam1", c, 5*time.Second, nil)
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	out := idx.SegmentsInWindow("cam1", start, time.Minute)
	require.Len(t, out, 3)
	require.GreaterOrEqual(t, idx.MemStats().UniqueTrackPtrs, 2, "new stream init after enable must be interned")
	idx.ClosePersist()
}

func TestRebuildOnlyDamagedDay(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(cam, 0o755))
	d1 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	d2 := time.Date(2020, 1, 2, 0, 0, 0, 0, time.Local)
	a := filepath.Join(cam, d1.Format("2006-01-02_15-04-05")+"-000000.mp4")
	b := filepath.Join(cam, d1.Add(5*time.Second).Format("2006-01-02_15-04-05")+"-000000.mp4")
	c := filepath.Join(cam, d2.Format("2006-01-02_15-04-05")+"-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 2)
	writeNamedFMP4(t, c, 2)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).Segments)
	require.Equal(t, 3, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	layout := makeDvrLayout(pathConf, "cam1")
	day1 := d1.Format("2006-01-02")
	day2 := d2.Format("2006-01-02")
	require.FileExists(t, layout.dayJournal(day1))
	require.FileExists(t, layout.dayJournal(day2))
	require.NoError(t, os.WriteFile(layout.dayJournal(day2), []byte("broken"), 0o644))

	idx2 := NewIndex()
	st := idx2.LoadFromDisk(confs)
	require.Equal(t, 1, st.DiskPaths, "meta still healthy → path loads from disk")
	require.False(t, idx2.pathNeedsRebuild("cam1"))
	idx2.PrefetchDays(nil)
	require.True(t, idx2.HasPendingDayRepairs())

	st = idx2.ReconcileAll(nil, false)
	require.Equal(t, 1, st.Built)
	require.False(t, idx2.HasPendingDayRepairs())
	require.FileExists(t, layout.dayJournal(day2))

	out1 := idx2.SegmentsInWindow("cam1", d1, time.Minute)
	require.Len(t, out1, 2)
	out2 := idx2.SegmentsInWindow("cam1", d2, time.Minute)
	require.Len(t, out2, 1)
	idx2.ClosePersist()
}

func TestRebuildArchivePlaylistNotEmpty(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	require.NoError(t, os.MkdirAll(cam, 0o755))
	a := filepath.Join(cam, "2020-01-01_12-00-00-000000.mp4")
	b := filepath.Join(cam, "2020-01-01_12-00-05-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 2)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).Segments)
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)
	idx.MarkNeedsRebuild("cam1")
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)

	start := time.Date(2020, 1, 1, 12, 0, 0, 0, time.Local)
	out := idx.SegmentsInWindow("cam1", start, time.Minute)
	require.Len(t, out, 2)
	body := GenerateArchiveM3U8Indexed(conf.RecordFormatFMP4, out, 5*time.Second, 0, 0, start)
	require.Contains(t, body, "#EXTINF:")
	require.Contains(t, body, filepath.Base(a))
	idx.ClosePersist()
}
