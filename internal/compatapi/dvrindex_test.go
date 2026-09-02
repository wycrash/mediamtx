package compatapi

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/stretchr/testify/require"
)

func testH264Track() *fmp4.InitTrack {
	return &fmp4.InitTrack{
		ID:        1,
		TimeScale: 1000,
		Codec: &mcodecs.H264{
			SPS: []byte{
				0x67, 0x64, 0x00, 0x1e, 0xac, 0xd9, 0x40, 0xa0,
				0x3d, 0xa1, 0x00, 0x00, 0x03, 0x00, 0x01, 0x00,
				0x00, 0x03, 0x00, 0x32, 0x8f, 0x18, 0x32, 0x48,
			},
			PPS: []byte{0x68, 0xee, 0x3c, 0xb0},
		},
	}
}

func writeNamedFMP4(t *testing.T, path string, moofs int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	init := fmp4.Init{Tracks: []*fmp4.InitTrack{testH264Track()}}
	require.NoError(t, init.Marshal(f))
	for i := 0; i < moofs; i++ {
		part := fmp4.Part{
			SequenceNumber: uint32(i),
			Tracks: []*fmp4.PartTrack{{
				ID:       1,
				BaseTime: uint64(i * 1000),
				Samples:  []*fmp4.Sample{{Duration: 1000, Payload: []byte{0, 0, 0, 1, 9, 0xf0}}},
			}},
		}
		require.NoError(t, part.Marshal(f))
	}
}

func TestDvrSnapshotRoundTrip(t *testing.T) {
	tracks := []*fmp4.InitTrack{testH264Track()}
	in := dvrSnapshot{
		Hash:   0x1122334455667788,
		Codecs: [][]*fmp4.InitTrack{tracks},
		Segs: []dvrSegRec{{
			Rel:      "2020-01-01_00-00-00-000000.mp4",
			Start:    time.Unix(1577836800, 0).UTC(),
			Duration: 5 * time.Second,
			Moof:     5,
			CodecID:  1,
			Ready:    true,
		}},
	}
	raw, err := encodeSnapshot(in)
	require.NoError(t, err)
	out, err := decodeSnapshot(raw)
	require.NoError(t, err)
	require.Equal(t, in.Hash, out.Hash)
	require.Len(t, out.Codecs, 1)
	require.True(t, fmp4TracksCompatible(tracks, out.Codecs[0]))
	require.Len(t, out.Segs, 1)
	require.Equal(t, in.Segs[0].Rel, out.Segs[0].Rel)
	require.Equal(t, in.Segs[0].Duration, out.Segs[0].Duration)
	require.Equal(t, in.Segs[0].Moof, out.Segs[0].Moof)
	require.True(t, in.Segs[0].Start.Equal(out.Segs[0].Start))
}

func TestDvrMetaRoundTrip(t *testing.T) {
	in := dvrMeta{
		Hash:   0xabc,
		Ranges: []RecordingRange{{From: 1000, Duration: 60}},
		Days:   []dvrDayInfo{{Date: "2020-01-01", NSeg: 12}},
	}
	raw := encodeMeta(in)
	out, err := decodeMeta(raw)
	require.NoError(t, err)
	require.Equal(t, in.Hash, out.Hash)
	require.Equal(t, in.Ranges, out.Ranges)
	require.Equal(t, in.Days, out.Days)
}

func TestDvrJournalTruncatedStops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mtx-dvr-index.journal")
	hash := uint64(99)
	op, err := encodeJournalOp(dvrJournalOp{
		Op: dvrOpUpsert,
		Seg: dvrSegRec{
			Rel:      "a.mp4",
			Start:    time.Unix(1000, 0).UTC(),
			Duration: time.Second,
			Ready:    true,
		},
	})
	require.NoError(t, err)
	data := append(journalHeader(hash), op...)
	data = append(data, 10, 0, 0, 0) // length prefix of a truncated record
	require.NoError(t, os.WriteFile(path, data, 0o644))

	ops, err := readJournalFile(path, hash)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	require.Equal(t, "a.mp4", ops[0].Seg.Rel)
}

func TestIndexLoadFromDiskUsesSnapshot(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	a := filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(cam, "2020-01-01_00-00-05-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 3)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx1 := NewIndex()
	st1 := idx1.LoadFromDisk(confs)
	require.Equal(t, 0, st1.Segments)
	require.Equal(t, 0, st1.DiskPaths)
	require.Equal(t, 0, st1.Inspected)
	st1 = idx1.ReconcileAll(nil, false)
	require.Equal(t, 2, st1.Segments)
	require.Equal(t, 1, st1.Inspected)
	idx1.ClosePersist()

	layout := makeDvrLayout(pathConf, "cam1")
	require.FileExists(t, layout.meta)
	require.FileExists(t, layout.daySnap("2020-01-01"))

	idx2 := NewIndex()
	st2 := idx2.LoadFromDisk(confs)
	require.Equal(t, 2, st2.Segments)
	require.Equal(t, 1, st2.DiskPaths)
	require.Equal(t, 0, st2.Inspected)

	out := idx2.SegmentsInWindow("cam1", time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local), time.Minute)
	require.Len(t, out, 2)
	require.True(t, out[0].fmp4.Ready)
	require.True(t, out[1].fmp4.Ready)
	require.Equal(t, 5*time.Second, out[0].fmp4.Duration)
	require.Equal(t, uint32(3), out[1].fmp4.MoofCount)
	require.Equal(t, 1, idx2.MemStats().UniqueTrackPtrs)
	idx2.ClosePersist()
}

func TestIndexRebuildsWhenSnapshotDeleted(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	a := filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(cam, "2020-01-01_00-00-05-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 3)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	layout := makeDvrLayout(pathConf, "cam1")
	require.NoError(t, os.Remove(layout.meta))
	require.NoError(t, os.Remove(layout.daySnap("2020-01-01")))
	_ = os.Remove(layout.dayJournal("2020-01-01"))

	idx = NewIndex()
	st := idx.LoadFromDisk(confs)
	require.Equal(t, 0, st.Segments)
	require.Equal(t, 0, st.DiskPaths)
	require.True(t, st.DiskPaths < st.Paths)

	// slow=true is what the periodic scheduler uses; a deleted index must
	// still rebuild immediately without that throttle.
	st = idx.ReconcileAll(nil, true)
	require.Equal(t, 2, st.Segments)
	require.Equal(t, 1, st.Built)
	require.Equal(t, 1, st.Inspected)
	out := idx.SegmentsInWindow("cam1", time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local), time.Minute)
	require.Len(t, out, 2)
	idx.ClosePersist()
}

func TestIndexLoadFromDiskReconcilesDeletedAndNew(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	a := filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(cam, "2020-01-01_00-00-05-000000.mp4")
	c := filepath.Join(cam, "2020-01-01_00-00-10-000000.mp4")
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
	idx.ClosePersist()

	require.NoError(t, os.Remove(b))
	writeNamedFMP4(t, c, 4)

	idx = NewIndex()
	st := idx.LoadFromDisk(confs)
	require.Equal(t, 2, st.Segments)
	require.Equal(t, 1, st.DiskPaths)
	require.Equal(t, 0, st.Inspected)
	require.Equal(t, 0, st.Removed)
	require.Equal(t, 0, st.Added)
	_, ok := idx.FindByName("cam1", filepath.Base(b))
	require.True(t, ok, "startup trusts snapshot and does not drop deleted files yet")

	st = idx.ReconcileAll(nil, false)
	require.Equal(t, 0, st.Built)
	require.Equal(t, 0, st.Inspected)
	require.Equal(t, 1, st.Removed)
	require.Equal(t, 1, st.Added)

	_, ok = idx.FindByName("cam1", filepath.Base(b))
	require.False(t, ok)
	fpath, ok := idx.FindByName("cam1", filepath.Base(c))
	require.True(t, ok)
	require.Equal(t, c, fpath)
	segs := idx.SegmentsInWindow("cam1", time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local), time.Minute)
	require.Len(t, segs, 2)
	var foundC bool
	for _, s := range segs {
		if s.Name() == filepath.Base(c) {
			foundC = true
			require.True(t, s.fmp4.Ready)
			require.Equal(t, 5*time.Second, s.fmp4.Duration)
		}
	}
	require.True(t, foundC)
	idx.ClosePersist()
}

func TestIndexPersistUpsertReplayedFromJournal(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	a := filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(cam, "2020-01-01_00-00-05-000000.mp4")
	writeNamedFMP4(t, a, 2)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).Segments)
	require.Equal(t, 1, idx.ReconcileAll(nil, false).Segments)

	writeNamedFMP4(t, b, 3)
	start := time.Date(2020, 1, 1, 0, 0, 5, 0, time.Local)
	idx.Add("cam1", b, start)
	meta, tracks, err := inspectFMP4Segment(b)
	require.NoError(t, err)
	idx.SetFMP4Meta("cam1", b, meta, tracks)
	idx.PersistUpsert("cam1", b)
	// do not ClosePersist: snapshot still has 1 file, journal has the upsert

	idx2 := NewIndex()
	st := idx2.LoadFromDisk(confs)
	require.Equal(t, 2, st.Segments)
	require.Equal(t, 1, st.DiskPaths)
	out := idx2.SegmentsInWindow("cam1", time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local), time.Minute)
	require.Len(t, out, 2)
	require.Equal(t, uint32(3), out[1].fmp4.MoofCount)
	idx2.ClosePersist()
}

func TestIndexReloadPathConfsLoadsSnapshot(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	a := filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(cam, "2020-01-01_00-00-05-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 3)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx1 := NewIndex()
	require.Equal(t, 0, idx1.LoadFromDisk(confs).Segments)
	require.Equal(t, 2, idx1.ReconcileAll(nil, false).Segments)
	idx1.ClosePersist()

	idx2 := NewIndex()
	require.Equal(t, 0, idx2.LoadFromDisk(nil).Segments)
	st := idx2.ReloadPathConfs(confs)
	require.Equal(t, 1, st.Paths)
	require.Equal(t, 1, st.DiskPaths)
	require.Equal(t, 2, st.Segments)
	out := idx2.SegmentsInWindow("cam1", time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local), time.Minute)
	require.Len(t, out, 2)
	require.True(t, out[0].fmp4.Ready)
	require.True(t, out[1].fmp4.Ready)
	require.Equal(t, uint32(3), out[1].fmp4.MoofCount)
	idx2.ClosePersist()
}

func datedFMP4(t *testing.T, dir, pathName string, start time.Time, moofs int) string {
	t.Helper()
	name := start.Format("2006-01-02_15-04-05") + "-000000.mp4"
	fpath := filepath.Join(dir, pathName, name)
	writeNamedFMP4(t, fpath, moofs)
	return fpath
}

func testRecordPathConf(dir, name string) *conf.Path {
	return &conf.Path{
		Name:                  name,
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
		RecordPartDuration:    conf.Duration(time.Second),
	}
}

func TestIndexRebuildsOtherPathsWhenLiveSegmentsArriveFirst(t *testing.T) {
	dir := t.TempDir()
	hist := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	cam1a := datedFMP4(t, dir, "cam1", hist, 2)
	cam1b := datedFMP4(t, dir, "cam1", hist.Add(5*time.Second), 2)
	cam2a := datedFMP4(t, dir, "cam2", hist, 2)
	cam2b := datedFMP4(t, dir, "cam2", hist.Add(5*time.Second), 3)

	confs := map[string]*conf.Path{
		"cam1": testRecordPathConf(dir, "cam1"),
		"cam2": testRecordPathConf(dir, "cam2"),
	}

	idx := NewIndex()
	st := idx.LoadFromDisk(confs)
	require.Equal(t, 0, st.Segments)
	require.Equal(t, 0, st.DiskPaths)
	require.True(t, st.DiskPaths < st.Paths)

	// While cam1 is still being rebuilt, live recording already indexed cam2.
	liveStart := time.Now().Truncate(time.Second)
	live := datedFMP4(t, dir, "cam2", liveStart, 2)
	idx.CompleteSegment("cam2", live, 5*time.Second)
	_, ok := idx.FindByName("cam2", filepath.Base(live))
	require.True(t, ok)
	require.Len(t, idx.SegmentsInWindow("cam2", liveStart.Add(-time.Minute), 2*time.Minute), 1)

	st = idx.ReconcileAll(nil, false)
	require.Equal(t, 2, st.Built)

	out1 := idx.SegmentsInWindow("cam1", hist, time.Minute)
	require.Len(t, out1, 2)
	require.Equal(t, filepath.Base(cam1a), out1[0].Name())
	require.Equal(t, filepath.Base(cam1b), out1[1].Name())

	out2 := idx.SegmentsInWindow("cam2", hist, time.Minute)
	require.Len(t, out2, 2, "historical cam2 segments must be rebuilt, not only the live edge")
	require.Equal(t, filepath.Base(cam2a), out2[0].Name())
	require.Equal(t, filepath.Base(cam2b), out2[1].Name())
	_, ok = idx.FindByName("cam2", filepath.Base(live))
	require.True(t, ok)
	idx.ClosePersist()
}

func TestIndexRebuildsWhenSnapshotCorruptAndLiveSegmentsExist(t *testing.T) {
	dir := t.TempDir()
	hist := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	datedFMP4(t, dir, "cam1", hist, 2)
	datedFMP4(t, dir, "cam1", hist.Add(5*time.Second), 2)
	datedFMP4(t, dir, "cam2", hist, 2)
	datedFMP4(t, dir, "cam2", hist.Add(5*time.Second), 2)

	confs := map[string]*conf.Path{
		"cam1": testRecordPathConf(dir, "cam1"),
		"cam2": testRecordPathConf(dir, "cam2"),
	}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 4, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	snap1 := makeDvrLayout(confs["cam1"], "cam1").meta
	snap2 := makeDvrLayout(confs["cam2"], "cam2").meta
	require.NoError(t, os.WriteFile(snap1, []byte("not-an-index"), 0o644))
	require.NoError(t, os.WriteFile(snap2, []byte("not-an-index"), 0o644))

	idx = NewIndex()
	st := idx.LoadFromDisk(confs)
	require.Equal(t, 0, st.DiskPaths)
	require.True(t, st.DiskPaths < st.Paths)

	live := datedFMP4(t, dir, "cam2", time.Now().Truncate(time.Second), 2)
	idx.CompleteSegment("cam2", live, 5*time.Second)

	st = idx.ReconcileAll(nil, true)
	require.Equal(t, 2, st.Built)
	require.Len(t, idx.SegmentsInWindow("cam1", hist, time.Minute), 2)
	require.Len(t, idx.SegmentsInWindow("cam2", hist, time.Minute), 2)
	_, ok := idx.FindByName("cam2", filepath.Base(live))
	require.True(t, ok)
	idx.ClosePersist()
}

func TestIndexCompleteSegmentUsesRecorderDuration(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	a := filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(cam, "2020-01-01_00-00-05-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 3)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
		RecordPartDuration:    conf.Duration(time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	idx.LoadFromDisk(confs)
	idx.CompleteSegment("cam1", a, 5*time.Second)
	idx.CompleteSegment("cam1", b, 5*time.Second)

	out := idx.SegmentsInWindow("cam1", time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local), time.Minute)
	require.Len(t, out, 2)
	require.Equal(t, 5*time.Second, out[0].fmp4.Duration)
	require.Equal(t, 5*time.Second, out[1].fmp4.Duration)
	require.Equal(t, 1, idx.MemStats().UniqueTrackPtrs)
	idx.ClosePersist()
}

func TestIndexDateDirStoresShardInDayFolder(t *testing.T) {
	dir := t.TempDir()
	hist := time.Date(2020, 1, 1, 12, 0, 0, 0, time.Local)
	day := hist.Format("2006-01-02")
	a := filepath.Join(dir, "cam1", day, hist.Format("2006-01-02_15-04-05")+"-000000.mp4")
	b := filepath.Join(dir, "cam1", day, hist.Add(5*time.Second).Format("2006-01-02_15-04-05")+"-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 3)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
		RecordPartDuration:    conf.Duration(time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	layout := makeDvrLayout(pathConf, "cam1")
	require.True(t, layout.dateDir)
	require.FileExists(t, layout.meta)
	require.FileExists(t, filepath.Join(dir, "cam1", day, ".mtx-dvr-index"))
	require.Equal(t, filepath.Join(dir, "cam1", day, ".mtx-dvr-index"), layout.daySnap(day))

	idx = NewIndex()
	st := idx.LoadFromDisk(confs)
	require.Equal(t, 1, st.DiskPaths)
	require.Equal(t, 2, st.Segments)
	out := idx.SegmentsInWindow("cam1", hist, time.Minute)
	require.Len(t, out, 2)
	idx.ClosePersist()
}

func TestIndexRangesSurviveLiveAddAndRepairFromDays(t *testing.T) {
	dir := t.TempDir()
	hist := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	datedFMP4(t, dir, "cam1", hist, 2)
	datedFMP4(t, dir, "cam1", hist.Add(5*time.Second), 2)

	pathConf := testRecordPathConf(dir, "cam1")
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)
	histRanges := idx.Ranges("cam1")
	require.NotEmpty(t, histRanges)
	require.LessOrEqual(t, histRanges[0].From, hist.Unix())

	live := datedFMP4(t, dir, "cam1", time.Now().Truncate(time.Second), 2)
	idx.AddFromPath("cam1", live)
	got := idx.Ranges("cam1")
	require.LessOrEqual(t, got[0].From, hist.Unix(), "live Add must not rebuild ranges from hot segments only")

	idx.ClosePersist()
	layout := makeDvrLayout(pathConf, "cam1")
	meta, err := readMetaFile(layout.meta)
	require.NoError(t, err)
	meta.Ranges = []RecordingRange{{From: time.Now().Add(-2 * time.Hour).Unix(), Duration: 3600}}
	require.NoError(t, writeMetaFile(layout.meta, meta))

	idx = NewIndex()
	require.Equal(t, 1, idx.LoadFromDisk(confs).DiskPaths)
	repaired := idx.Ranges("cam1")
	require.NotEmpty(t, repaired)
	require.LessOrEqual(t, repaired[0].From, hist.Unix(), "truncated meta ranges must be rebuilt from day shards")
	idx.ClosePersist()
}

func TestIndexLoadFromDiskTwoDisks(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	cam1 := filepath.Join(d1, "cam1")
	cam2 := filepath.Join(d2, "cam1")
	a := filepath.Join(cam1, "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(cam2, "2020-01-01_00-00-05-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 3)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            "%path/%Y-%m-%d_%H-%M-%S-%f",
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
		Storage:               "dvr",
		StorageDisks:          []string{d1, d2},
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx1 := NewIndex()
	require.Equal(t, 0, idx1.LoadFromDisk(confs).DiskPaths)
	st1 := idx1.ReconcileAll(nil, false)
	require.Equal(t, 2, st1.Segments)
	idx1.ClosePersist()

	layouts := makeDvrLayouts(pathConf, "cam1")
	require.Len(t, layouts, 2)
	require.FileExists(t, layouts[0].meta)
	require.FileExists(t, layouts[1].meta)
	require.FileExists(t, layouts[0].daySnap("2020-01-01"))
	require.FileExists(t, layouts[1].daySnap("2020-01-01"))

	idx2 := NewIndex()
	st2 := idx2.LoadFromDisk(confs)
	require.Equal(t, 2, st2.Segments)
	require.Equal(t, 1, st2.DiskPaths)

	out := idx2.SegmentsInWindow("cam1", time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local), time.Minute)
	require.Len(t, out, 2)
	require.Equal(t, a, out[0].Fpath())
	require.Equal(t, b, out[1].Fpath())
	idx2.ClosePersist()
}

func TestIndexRebuildsWhenOneStorageDiskIndexDeleted(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	a := filepath.Join(d1, "cam1", "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(d2, "cam1", "2020-01-01_00-00-05-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 3)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            "%path/%Y-%m-%d_%H-%M-%S-%f",
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
		Storage:               "dvr",
		StorageDisks:          []string{d1, d2},
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	layouts := makeDvrLayouts(pathConf, "cam1")
	require.NoError(t, os.Remove(layouts[0].meta))
	require.NoError(t, os.Remove(layouts[0].daySnap("2020-01-01")))
	_ = os.Remove(layouts[0].dayJournal("2020-01-01"))

	idx = NewIndex()
	st := idx.LoadFromDisk(confs)
	require.Equal(t, 0, st.DiskPaths, "one broken disk must not count the path as fully loaded")
	require.True(t, idx.pathNeedsRebuild("cam1"))

	st = idx.ReconcileAll(nil, true)
	require.Equal(t, 1, st.Built)
	require.Equal(t, 2, st.Segments)
	require.FileExists(t, layouts[0].meta)
	require.FileExists(t, layouts[0].daySnap("2020-01-01"))
	out := idx.SegmentsInWindow("cam1", time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local), time.Minute)
	require.Len(t, out, 2)
	idx.ClosePersist()
}

func TestIndexRebuildsWhenOneStorageDiskSnapshotsDeleted(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	a := filepath.Join(d1, "cam1", "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(d2, "cam1", "2020-01-01_00-00-05-000000.mp4")
	writeNamedFMP4(t, a, 2)
	writeNamedFMP4(t, b, 3)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            "%path/%Y-%m-%d_%H-%M-%S-%f",
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
		Storage:               "dvr",
		StorageDisks:          []string{d1, d2},
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 2, idx.ReconcileAll(nil, false).Segments)
	idx.ClosePersist()

	layouts := makeDvrLayouts(pathConf, "cam1")
	require.NoError(t, os.Remove(layouts[0].daySnap("2020-01-01")))
	_ = os.Remove(layouts[0].dayJournal("2020-01-01"))
	require.FileExists(t, layouts[0].meta)

	idx = NewIndex()
	st := idx.LoadFromDisk(confs)
	require.Equal(t, 0, st.DiskPaths)
	require.True(t, idx.pathNeedsRebuild("cam1"))
	st = idx.ReconcileAll(nil, true)
	require.Equal(t, 1, st.Built)
	require.FileExists(t, layouts[0].daySnap("2020-01-01"))
	out := idx.SegmentsInWindow("cam1", time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local), time.Minute)
	require.Len(t, out, 2)
	idx.ClosePersist()
}

func TestIndexRecordingStatusMergesRoundRobinDisks(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	a := filepath.Join(d1, "cam1", "2020-01-01_00-00-00-000000.mp4")
	b := filepath.Join(d2, "cam1", "2020-01-01_00-00-05-000000.mp4")
	c := filepath.Join(d1, "cam1", "2020-01-01_00-00-10-000000.mp4")
	writeNamedFMP4(t, a, 5)
	writeNamedFMP4(t, b, 5)
	writeNamedFMP4(t, c, 5)

	pathConf := &conf.Path{
		Name:                  "cam1",
		RecordPath:            "%path/%Y-%m-%d_%H-%M-%S-%f",
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
		Storage:               "dvr",
		StorageDisks:          []string{d1, d2},
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx := NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 3, idx.ReconcileAll(nil, false).Segments)
	ranges := idx.Ranges("cam1")
	require.Len(t, ranges, 1, "round-robin files on two disks must look continuous in recording_status")
	require.Equal(t, time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local).Unix(), ranges[0].From)
	require.Equal(t, int64(15), ranges[0].Duration)

	layouts := makeDvrLayouts(pathConf, "cam1")
	require.NoError(t, os.Remove(layouts[1].meta))
	require.NoError(t, os.Remove(layouts[1].daySnap("2020-01-01")))
	_ = os.Remove(layouts[1].dayJournal("2020-01-01"))
	idx.ClosePersist()

	idx = NewIndex()
	require.Equal(t, 0, idx.LoadFromDisk(confs).DiskPaths)
	require.Equal(t, 3, idx.ReconcileAll(nil, false).Segments)
	ranges = idx.Ranges("cam1")
	require.Len(t, ranges, 1)
	require.Equal(t, int64(15), ranges[0].Duration)
	idx.ClosePersist()
}
