package compatapi

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

const (
	reconcileWalkBatch    = 20
	reconcileWalkPause    = 20 * time.Millisecond
	reconcileInspectPause = 40 * time.Millisecond
	reconcileEdgeWindow   = 24 * time.Hour
)

var errReconcileStop = errors.New("dvr index reconcile stopped")

// fmp4SegMeta is cached fMP4 playlist metadata so m3u8 generation does not touch disk.
type fmp4SegMeta struct {
	Duration  time.Duration
	MoofCount uint32
	Tracks    []*fmp4.InitTrack
	Ready     bool
}

// IndexedSegment is a recording segment tracked in memory.
type IndexedSegment struct {
	Fpath string
	Start time.Time
	Name  string
	fmp4  fmp4SegMeta
}

type pathIndex struct {
	segments        []*IndexedSegment
	byName          map[string]*IndexedSegment
	ranges          []RecordingRange
	rangesOK        bool
	segmentDuration time.Duration
	internedTracks  [][]*fmp4.InitTrack
	persist         *dvrPersist
	commonPath      string
	// complete is true after a trusted snapshot load or a finished directory
	// rebuild. Live OnSegmentComplete inserts must not set this: otherwise a
	// path that is still missing its on-disk index looks non-empty and only
	// the new edge is indexed while other cameras are still rebuilding.
	complete bool
}

// IndexLoadStats is returned by Index.LoadFromDisk.
type IndexLoadStats struct {
	Paths     int
	Segments  int
	DiskPaths int
	Inspected int
	Removed   int
	Added     int
	Built     int
}

// Index keeps an in-memory inventory of recording segments.
type Index struct {
	mutex     sync.RWMutex
	paths     map[string]*pathIndex
	pathConfs map[string]*conf.Path
	persistOK bool
}

// NewIndex allocates an Index.
func NewIndex() *Index {
	return &Index{
		paths: make(map[string]*pathIndex),
	}
}

// ReloadPathConfs updates path configuration used for decoding / durations.
// Paths that were not previously indexed load their on-disk snapshot+journal,
// the same way LoadFromDisk does at startup. Otherwise an API-added path whose
// recordings already exist on disk stays empty until the next reconcile.
func (idx *Index) ReloadPathConfs(pathConfs map[string]*conf.Path) IndexLoadStats {
	var st IndexLoadStats
	idx.mutex.Lock()
	idx.pathConfs = pathConfs
	known := make(map[string]struct{}, len(idx.paths))
	for name, pe := range idx.paths {
		known[name] = struct{}{}
		if pathConf, _, err := conf.FindPathConf(pathConfs, name); err == nil {
			dur := time.Duration(pathConf.RecordSegmentDuration)
			if pe.segmentDuration != dur {
				pe.segmentDuration = dur
				pe.rangesOK = false
			}
		}
	}
	idx.mutex.Unlock()

	before := idx.SegmentCount()
	for _, pathName := range recordingPathNames(pathConfs) {
		if _, ok := known[pathName]; ok {
			continue
		}
		pathConf, _, err := conf.FindPathConf(pathConfs, pathName)
		if err != nil {
			continue
		}
		st.Paths++
		ps := idx.loadPath(pathConf, pathName)
		if ps.fromDisk {
			st.DiskPaths++
		}
	}
	st.Segments = idx.SegmentCount() - before
	return st
}

// LoadFromDisk loads snapshot+journal if present. It never walks recordings:
// the HTTP API can start immediately. A missing or broken index is rebuilt
// immediately by ReconcileAll without I/O throttling.
func (idx *Index) LoadFromDisk(pathConfs map[string]*conf.Path) IndexLoadStats {
	var st IndexLoadStats
	idx.mutex.Lock()
	idx.pathConfs = pathConfs
	idx.paths = make(map[string]*pathIndex)
	idx.persistOK = true
	idx.mutex.Unlock()

	pathNames := recordingPathNames(pathConfs)
	st.Paths = len(pathNames)
	for _, pathName := range pathNames {
		pathConf, _, err := conf.FindPathConf(pathConfs, pathName)
		if err != nil {
			continue
		}
		ps := idx.loadPath(pathConf, pathName)
		if ps.fromDisk {
			st.DiskPaths++
		}
	}
	st.Segments = idx.SegmentCount()
	return st
}

func recordingPathNames(pathConfs map[string]*conf.Path) []string {
	names := make(map[string]struct{})
	for _, pathConf := range pathConfs {
		if pathConf == nil {
			continue
		}
		if pathConf.Regexp == nil {
			if pathConf.Name != "" {
				names[pathConf.Name] = struct{}{}
			}
			continue
		}
		for name := range regexpPathNamesFromDirs(pathConf) {
			names[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func regexpPathNamesFromDirs(pathConf *conf.Path) map[string]struct{} {
	ret := make(map[string]struct{})
	recordPath := recordstore.PathAddExtension(pathConf.RecordPath, pathConf.RecordFormat)
	recordPath, _ = filepath.Abs(recordPath)
	common := recordstore.CommonPath(recordPath)
	if common == "" {
		return ret
	}
	entries, err := os.ReadDir(common)
	if err != nil {
		return ret
	}
	hasPathVar := strings.Contains(pathConf.RecordPath, "%path")
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, dvrSnapName) {
			if name == dvrSnapName || strings.HasSuffix(name, dvrJournalSuffix) {
				continue
			}
			if strings.HasPrefix(name, dvrSnapName+".") {
				pname := strings.TrimPrefix(name, dvrSnapName+".")
				if pathConf.Regexp != nil && pathConf.Regexp.MatchString(pname) {
					ret[pname] = struct{}{}
				}
			}
			continue
		}
		if hasPathVar && e.IsDir() {
			if err := conf.IsValidPathName(name); err != nil {
				continue
			}
			if pathConf.Regexp != nil && pathConf.Regexp.MatchString(name) {
				ret[name] = struct{}{}
			}
		}
	}
	return ret
}

type pathLoadStats struct {
	fromDisk bool
}

func (idx *Index) loadPath(pathConf *conf.Path, pathName string) pathLoadStats {
	var st pathLoadStats
	p := newDvrPersist(pathConf, pathName)
	_, _, common := dvrIndexPaths(pathConf, pathName)
	snap, snapErr := readSnapshotFile(p.snapPath)
	if snapErr != nil || snap.Hash != p.hash {
		snap = dvrSnapshot{}
		snapErr = errDvrIndexBadMagic
	}
	ops, _ := readJournalFile(p.journalPath, p.hash)
	hasDisk := snapErr == nil || len(ops) > 0

	if hasDisk && snapErr == nil {
		idx.applySnapshot(pathName, common, p, snap)
		idx.applyJournal(pathName, common, ops)
		st.fromDisk = true
	} else if hasDisk && len(ops) > 0 {
		// Journal without a trusted snapshot is a partial index (corrupt or
		// never compacted). Keep the segments for immediate API use, but do
		// not mark the path complete: ReconcileAll must still walk history.
		idx.mutex.Lock()
		pe := idx.ensurePathLocked(pathName)
		pe.persist = p
		pe.commonPath = common
		idx.mutex.Unlock()
		idx.applyJournal(pathName, common, ops)
	}

	if st.fromDisk || (hasDisk && len(ops) > 0) {
		p.journalOps = len(ops)
		_ = p.openJournalAppend()
	}
	return st
}

// ReconcileAll repairs the index without re-reading known files.
// A path with no trusted snapshot (deleted, corrupt, or never built) is listed
// from filenames immediately, even if live recording already inserted a few
// new segments. A healthy index is checked only at the archive edges.
func (idx *Index) ReconcileAll(stop <-chan struct{}, slow bool) IndexLoadStats {
	var st IndexLoadStats
	if stopped(stop) {
		return st
	}
	idx.mutex.RLock()
	pathConfs := idx.pathConfs
	idx.mutex.RUnlock()
	if pathConfs == nil {
		return st
	}

	pathNames := recordingPathNames(pathConfs)
	st.Paths = len(pathNames)
	for _, pathName := range pathNames {
		if stopped(stop) {
			break
		}
		pathConf, _, err := conf.FindPathConf(pathConfs, pathName)
		if err != nil {
			continue
		}
		var ins, add, del int
		if idx.pathNeedsRebuild(pathName) {
			ins, add, del = idx.buildPathFromDir(pathName, pathConf, stop, false)
			if add > 0 {
				st.Built++
			}
		} else {
			ins, add, del = idx.reconcilePathEdges(pathName, pathConf, stop, slow)
		}
		st.Inspected += ins
		st.Added += add
		st.Removed += del
	}
	st.Segments = idx.SegmentCount()
	st.DiskPaths = st.Paths
	return st
}

func (idx *Index) pathNeedsRebuild(pathName string) bool {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	pe := idx.paths[pathName]
	return pe == nil || !pe.complete || len(pe.segments) == 0
}

func (idx *Index) applySnapshot(pathName string, common string, p *dvrPersist, snap dvrSnapshot) {
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe := idx.ensurePathLocked(pathName)
	pe.persist = p
	pe.commonPath = common
	pe.complete = true
	pe.internedTracks = append([][]*fmp4.InitTrack(nil), snap.Codecs...)
	for i := range pe.internedTracks {
		p.savedCodec[uint8(i+1)] = struct{}{}
	}
	for _, rec := range snap.Segs {
		fpath := dvrAbsPath(common, rec.Rel)
		idx.addLocked(pe, fpath, rec.Start)
		idx.applySegMetaLocked(pe, fpath, rec)
	}
}

func (idx *Index) applyJournal(pathName, common string, ops []dvrJournalOp) {
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe := idx.paths[pathName]
	if pe == nil {
		return
	}
	for _, op := range ops {
		switch op.Op {
		case dvrOpCodec:
			id := int(op.CodecID)
			if id <= 0 {
				continue
			}
			for len(pe.internedTracks) < id {
				pe.internedTracks = append(pe.internedTracks, nil)
			}
			pe.internedTracks[id-1] = op.Tracks
			if pe.persist != nil {
				pe.persist.savedCodec[op.CodecID] = struct{}{}
			}
		case dvrOpUpsert:
			fpath := dvrAbsPath(common, op.Seg.Rel)
			idx.addLocked(pe, fpath, op.Seg.Start)
			idx.applySegMetaLocked(pe, fpath, op.Seg)
		case dvrOpDelete:
			idx.removeRelLocked(pe, op.Seg.Rel)
		}
	}
}

func (idx *Index) applySegMetaLocked(pe *pathIndex, fpath string, rec dvrSegRec) {
	name := filepath.Base(fpath)
	seg, ok := pe.byName[name]
	if !ok {
		return
	}
	meta := fmp4SegMeta{Duration: rec.Duration, MoofCount: rec.Moof, Ready: rec.Ready}
	if rec.CodecID > 0 && int(rec.CodecID) <= len(pe.internedTracks) {
		meta.Tracks = pe.internedTracks[rec.CodecID-1]
	}
	if !meta.Ready && (meta.Duration > 0 || meta.MoofCount > 0 || len(meta.Tracks) > 0) {
		meta.Ready = true
	}
	seg.fmp4 = meta
	pe.rangesOK = false
}

func (idx *Index) buildPathFromDir(
	pathName string,
	pathConf *conf.Path,
	stop <-chan struct{},
	slow bool,
) (inspected, added, removed int) {
	p := newDvrPersist(pathConf, pathName)
	_, _, common := dvrIndexPaths(pathConf, pathName)
	if common == "" {
		return 0, 0, 0
	}
	recordPath := recordstore.PathAddExtension(
		strings.ReplaceAll(pathConf.RecordPath, "%path", pathName),
		pathConf.RecordFormat,
	)
	recordPath, _ = filepath.Abs(recordPath)

	idx.mutex.Lock()
	pe := idx.ensurePathLocked(pathName)
	if pe.persist == nil {
		pe.persist = p
		pe.commonPath = common
	} else {
		p = pe.persist
		if pe.commonPath == "" {
			pe.commonPath = common
		}
	}
	idx.mutex.Unlock()
	_ = p.openJournalAppend()

	nWalk := 0
	var newest string
	var newestStart time.Time
	walkErr := filepath.WalkDir(common, func(fpath string, info fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if stopped(stop) {
			return errReconcileStop
		}
		nWalk++
		if slow && nWalk%reconcileWalkBatch == 0 {
			if !sleepOrStop(stop, reconcileWalkPause) {
				return errReconcileStop
			}
		}
		if info.IsDir() || strings.HasPrefix(info.Name(), dvrSnapName) {
			return nil
		}
		var pa recordstore.Path
		if !pa.Decode(recordPath, fpath) {
			return nil
		}
		idx.Add(pathName, fpath, pa.Start)
		added++
		if newestStart.IsZero() || pa.Start.After(newestStart) {
			newestStart = pa.Start
			newest = fpath
		}
		return nil
	})
	finished := walkErr == nil || os.IsNotExist(walkErr)
	if walkErr != nil && !errors.Is(walkErr, errReconcileStop) && !os.IsNotExist(walkErr) {
		return 0, added, 0
	}
	if added == 0 {
		return 0, 0, 0
	}

	nominal := time.Duration(pathConf.RecordSegmentDuration)
	part := time.Duration(pathConf.RecordPartDuration)
	if pathConf.RecordFormat == conf.RecordFormatFMP4 && newest != "" && idx.internedTracksOf(pathName) == nil {
		if meta, err := inspectFMP4Segment(newest); err == nil && meta.Ready {
			idx.SetFMP4Meta(pathName, newest, meta)
			inspected = 1
			if slow {
				_ = sleepOrStop(stop, reconcileInspectPause)
			}
		}
	}
	idx.fillMetaFromGaps(pathName, nominal, part)
	idx.compactPath(pathName)
	if finished {
		idx.mutex.Lock()
		if pe := idx.paths[pathName]; pe != nil {
			pe.complete = true
		}
		idx.mutex.Unlock()
	}
	return inspected, added, 0
}

func (idx *Index) reconcilePathEdges(
	pathName string,
	pathConf *conf.Path,
	stop <-chan struct{},
	slow bool,
) (inspected, added, removed int) {
	idx.mutex.Lock()
	pe := idx.paths[pathName]
	if pe == nil || len(pe.segments) == 0 {
		idx.mutex.Unlock()
		return 0, 0, 0
	}
	idx.ensurePersistLocked(pathName, pe)
	nominal := pe.segmentDuration
	if nominal <= 0 {
		nominal = time.Duration(pathConf.RecordSegmentDuration)
	}
	part := time.Duration(pathConf.RecordPartDuration)
	deleteAfter := time.Duration(pathConf.RecordDeleteAfter)
	idx.mutex.Unlock()

	removed += idx.pruneExpired(pathName, deleteAfter)
	removed += idx.dropMissingOldEdge(pathName, stop)
	if stopped(stop) {
		idx.compactPathIfDirty(pathName, added > 0 || removed > 0)
		return inspected, added, removed
	}

	n, ins, drop := idx.adoptNewEdge(pathName, pathConf, nominal, part, stop, slow)
	added += n
	inspected += ins
	removed += drop
	idx.compactPathIfDirty(pathName, added > 0 || inspected > 0 || removed > 0)
	return inspected, added, removed
}

func (idx *Index) pruneExpired(pathName string, deleteAfter time.Duration) int {
	if deleteAfter <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-deleteAfter)
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe := idx.paths[pathName]
	if pe == nil {
		return 0
	}
	removed := 0
	for len(pe.segments) > 0 {
		seg := pe.segments[0]
		if !seg.Start.Before(cutoff) {
			break
		}
		idx.persistDeleteLocked(pe, seg.Fpath)
		idx.removeRelLocked(pe, dvrRelPath(pe.commonPath, seg.Fpath))
		removed++
	}
	return removed
}

func (idx *Index) dropMissingOldEdge(pathName string, stop <-chan struct{}) int {
	removed := 0
	var windowEnd time.Time
	for {
		if stopped(stop) {
			return removed
		}
		idx.mutex.RLock()
		pe := idx.paths[pathName]
		if pe == nil || len(pe.segments) == 0 {
			idx.mutex.RUnlock()
			return removed
		}
		seg := pe.segments[0]
		fpath := seg.Fpath
		start := seg.Start
		idx.mutex.RUnlock()
		if windowEnd.IsZero() {
			windowEnd = start.Add(reconcileEdgeWindow)
		}
		if start.After(windowEnd) {
			return removed
		}
		err := fileExists(fpath)
		if err == nil {
			return removed
		}
		if !os.IsNotExist(err) {
			return removed
		}
		idx.mutex.Lock()
		pe = idx.paths[pathName]
		if pe != nil {
			idx.persistDeleteLocked(pe, fpath)
			idx.removeRelLocked(pe, dvrRelPath(pe.commonPath, fpath))
			removed++
		}
		idx.mutex.Unlock()
	}
}

func (idx *Index) adoptNewEdge(
	pathName string,
	pathConf *conf.Path,
	nominal, part time.Duration,
	stop <-chan struct{},
	slow bool,
) (added, inspected, removed int) {
	if nominal <= 0 {
		nominal = time.Second
	}

	idx.mutex.RLock()
	pe := idx.paths[pathName]
	if pe == nil || len(pe.segments) == 0 {
		idx.mutex.RUnlock()
		return 0, 0, 0
	}
	tailLimit := pe.segments[len(pe.segments)-1].Start.Add(-reconcileEdgeWindow)
	idx.mutex.RUnlock()

	for {
		if stopped(stop) {
			return added, inspected, removed
		}
		idx.mutex.RLock()
		pe = idx.paths[pathName]
		if pe == nil || len(pe.segments) == 0 {
			idx.mutex.RUnlock()
			return added, inspected, removed
		}
		last := pe.segments[len(pe.segments)-1]
		lastStart := last.Start
		lastPath := last.Fpath
		idx.mutex.RUnlock()
		err := fileExists(lastPath)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return added, inspected, removed
		}
		if lastStart.Before(tailLimit) {
			break
		}
		idx.mutex.Lock()
		pe = idx.paths[pathName]
		if pe != nil {
			idx.persistDeleteLocked(pe, lastPath)
			idx.removeRelLocked(pe, dvrRelPath(pe.commonPath, lastPath))
			removed++
		}
		idx.mutex.Unlock()
	}

	idx.mutex.RLock()
	pe = idx.paths[pathName]
	if pe == nil || len(pe.segments) == 0 {
		idx.mutex.RUnlock()
		return added, inspected, removed
	}
	lastStart := pe.segments[len(pe.segments)-1].Start
	idx.mutex.RUnlock()

	now := time.Now()
	windows := [][2]time.Time{{lastStart.Add(nominal), minTime(now, lastStart.Add(reconcileEdgeWindow))}}
	if now.Sub(lastStart) > reconcileEdgeWindow {
		windows = append(windows, [2]time.Time{now.Add(-reconcileEdgeWindow), now})
	}

	nProbe := 0
	for _, w := range windows {
		from, to := w[0], w[1]
		if from.IsZero() || to.IsZero() || from.After(to) {
			continue
		}
		for t := from; !t.After(to); t = t.Add(nominal) {
			if stopped(stop) {
				return added, inspected, removed
			}
			nProbe++
			if slow && nProbe%reconcileWalkBatch == 0 {
				if !sleepOrStop(stop, reconcileWalkPause) {
					return added, inspected, removed
				}
			}
			fpath := encodeRecordFile(pathConf, pathName, t)
			if fpath == "" {
				continue
			}
			if err := fileExists(fpath); err != nil {
				continue
			}
			if _, ok := idx.FindByName(pathName, filepath.Base(fpath)); ok {
				continue
			}
			start, ok := idx.decodeStart(pathName, fpath)
			if !ok {
				start = t
			}
			if idx.adoptDiskSegment(pathName, pathConf, fpath, start, nominal, part) {
				added++
			}
		}
	}
	return added, inspected, removed
}

func (idx *Index) adoptDiskSegment(
	pathName string,
	pathConf *conf.Path,
	fpath string,
	start time.Time,
	nominal, part time.Duration,
) bool {
	idx.Add(pathName, fpath, start)
	tracks := idx.internedTracksOf(pathName)
	meta := fmp4SegMeta{
		Duration:  nominal,
		MoofCount: estimateMoofCount(nominal, part, nominal),
		Tracks:    tracks,
		Ready:     true,
	}
	if pathConf.RecordFormat == conf.RecordFormatFMP4 && len(tracks) == 0 {
		if ins, err := inspectFMP4Segment(fpath); err == nil && ins.Ready {
			meta = ins
		}
	}
	idx.SetFMP4Meta(pathName, fpath, meta)
	idx.PersistUpsert(pathName, fpath)
	return true
}

func (idx *Index) fillMetaFromGaps(pathName string, nominal, part time.Duration) {
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe := idx.paths[pathName]
	if pe == nil {
		return
	}
	var tracks []*fmp4.InitTrack
	for i := len(pe.internedTracks) - 1; i >= 0; i-- {
		if len(pe.internedTracks[i]) > 0 {
			tracks = pe.internedTracks[i]
			break
		}
	}
	if nominal <= 0 {
		nominal = pe.segmentDuration
	}
	for i, seg := range pe.segments {
		dur := nominal
		if i+1 < len(pe.segments) {
			if d := pe.segments[i+1].Start.Sub(seg.Start); d > 0 && d <= nominal*3 {
				dur = d
			}
		}
		if seg.fmp4.Duration > 0 {
			dur = seg.fmp4.Duration
		}
		if !seg.fmp4.Ready || seg.fmp4.Duration == 0 || (seg.fmp4.Tracks == nil && tracks != nil) {
			if seg.fmp4.MoofCount == 0 {
				seg.fmp4.MoofCount = estimateMoofCount(dur, part, nominal)
			}
			seg.fmp4.Duration = dur
			if seg.fmp4.Tracks == nil {
				seg.fmp4.Tracks = tracks
			}
			seg.fmp4.Ready = true
			pe.rangesOK = false
		}
	}
}

func (idx *Index) internedTracksOf(pathName string) []*fmp4.InitTrack {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	pe := idx.paths[pathName]
	if pe == nil {
		return nil
	}
	for i := len(pe.internedTracks) - 1; i >= 0; i-- {
		if len(pe.internedTracks[i]) > 0 {
			return pe.internedTracks[i]
		}
	}
	return nil
}

func estimateMoofCount(duration, part, nominal time.Duration) uint32 {
	if duration <= 0 {
		duration = nominal
	}
	step := part
	if step <= 0 {
		step = time.Second
	}
	if duration <= 0 {
		return 1
	}
	n := uint32((duration + step/2) / step)
	if n == 0 {
		n = 1
	}
	return n
}

func encodeRecordFile(pathConf *conf.Path, pathName string, start time.Time) string {
	if pathConf == nil || start.IsZero() {
		return ""
	}
	recordPath := recordstore.PathAddExtension(
		strings.ReplaceAll(pathConf.RecordPath, "%path", pathName),
		pathConf.RecordFormat,
	)
	recordPath, _ = filepath.Abs(recordPath)
	var pa recordstore.Path
	pa.Path = pathName
	pa.Start = start
	return pa.Encode(recordPath)
}

func fileExists(fpath string) error {
	if fpath == "" {
		return os.ErrNotExist
	}
	_, err := os.Stat(fpath)
	return err
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (idx *Index) compactPathIfDirty(pathName string, dirty bool) {
	if !dirty {
		idx.mutex.Lock()
		pe := idx.paths[pathName]
		if pe != nil && pe.persist != nil && pe.persist.journalOps > 0 {
			dirty = true
		}
		idx.mutex.Unlock()
	}
	if dirty {
		idx.compactPath(pathName)
	}
}

func stopped(stop <-chan struct{}) bool {
	if stop == nil {
		return false
	}
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

func sleepOrStop(stop <-chan struct{}, d time.Duration) bool {
	if d <= 0 {
		return !stopped(stop)
	}
	if stop == nil {
		time.Sleep(d)
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return false
	case <-t.C:
		return true
	}
}

// SegmentCount returns the number of indexed segments across all paths.
func (idx *Index) SegmentCount() int {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	n := 0
	for _, pe := range idx.paths {
		n += len(pe.segments)
	}
	return n
}

// SetFMP4Meta stores inspected fMP4 playlist metadata for a segment.
func (idx *Index) SetFMP4Meta(pathName, fpath string, meta fmp4SegMeta) {
	if pathName == "" || fpath == "" || !meta.Ready {
		return
	}
	name := filepath.Base(fpath)

	idx.mutex.Lock()
	defer idx.mutex.Unlock()

	pe := idx.paths[pathName]
	if pe == nil {
		return
	}
	seg, ok := pe.byName[name]
	if !ok {
		return
	}
	meta.Tracks = internTracks(pe, meta.Tracks)
	seg.Fpath = fpath
	seg.fmp4 = meta
	pe.rangesOK = false
}

func internTracks(pe *pathIndex, tracks []*fmp4.InitTrack) []*fmp4.InitTrack {
	if pe == nil || len(tracks) == 0 {
		return tracks
	}
	for _, existing := range pe.internedTracks {
		if len(existing) == 0 {
			continue
		}
		if fmp4TracksCompatible(existing, tracks) {
			return existing
		}
	}
	pe.internedTracks = append(pe.internedTracks, tracks)
	return tracks
}

func internCodecID(pe *pathIndex, tracks []*fmp4.InitTrack) uint8 {
	if pe == nil || len(tracks) == 0 {
		return 0
	}
	for i, existing := range pe.internedTracks {
		if len(existing) == 0 {
			continue
		}
		if sameTracksPtr(existing, tracks) || fmp4TracksCompatible(existing, tracks) {
			return uint8(i + 1)
		}
	}
	return 0
}

func sameTracksPtr(a, b []*fmp4.InitTrack) bool {
	return len(a) > 0 && len(b) > 0 && len(a) == len(b) && &a[0] == &b[0]
}

func (idx *Index) addLocked(pe *pathIndex, fpath string, start time.Time) {
	if pe == nil || fpath == "" || start.IsZero() {
		return
	}
	name := filepath.Base(fpath)
	if existing, ok := pe.byName[name]; ok {
		existing.Fpath = fpath
		if !existing.Start.Equal(start) {
			existing.Start = start
			sort.Slice(pe.segments, func(i, j int) bool {
				return pe.segments[i].Start.Before(pe.segments[j].Start)
			})
			pe.rangesOK = false
		}
		return
	}

	seg := &IndexedSegment{
		Fpath: fpath,
		Start: start,
		Name:  name,
	}
	pe.byName[name] = seg
	i := sort.Search(len(pe.segments), func(i int) bool {
		return !pe.segments[i].Start.Before(start)
	})
	pe.segments = append(pe.segments, nil)
	copy(pe.segments[i+1:], pe.segments[i:])
	pe.segments[i] = seg
	pe.rangesOK = false
}

func (idx *Index) removeRelLocked(pe *pathIndex, rel string) {
	if pe == nil || rel == "" {
		return
	}
	name := filepath.Base(rel)
	seg, ok := pe.byName[name]
	if !ok {
		return
	}
	if pe.commonPath != "" {
		want := dvrAbsPath(pe.commonPath, rel)
		if seg.Fpath != want && filepath.Base(seg.Fpath) != name {
			return
		}
	}
	delete(pe.byName, name)
	for i, s := range pe.segments {
		if s == seg {
			pe.segments = append(pe.segments[:i], pe.segments[i+1:]...)
			break
		}
	}
	pe.rangesOK = false
	fmp4InitSizeCache.Delete(seg.Fpath)
}

func (idx *Index) snapshotFromPathLocked(pe *pathIndex) dvrSnapshot {
	var s dvrSnapshot
	if pe.persist != nil {
		s.Hash = pe.persist.hash
	}
	s.Codecs = append([][]*fmp4.InitTrack(nil), pe.internedTracks...)
	s.Segs = make([]dvrSegRec, 0, len(pe.segments))
	for _, seg := range pe.segments {
		if !seg.fmp4.Ready {
			continue
		}
		rel := seg.Name
		if pe.commonPath != "" {
			rel = dvrRelPath(pe.commonPath, seg.Fpath)
		}
		s.Segs = append(s.Segs, dvrSegRec{
			Rel:      rel,
			Start:    seg.Start,
			Duration: seg.fmp4.Duration,
			Moof:     seg.fmp4.MoofCount,
			CodecID:  internCodecID(pe, seg.fmp4.Tracks),
			Ready:    true,
		})
	}
	return s
}

func (idx *Index) compactPath(pathName string) {
	idx.mutex.Lock()
	pe := idx.paths[pathName]
	if pe == nil || pe.persist == nil {
		idx.mutex.Unlock()
		return
	}
	snap := idx.snapshotFromPathLocked(pe)
	p := pe.persist
	idx.mutex.Unlock()

	err := writeSnapshotFile(p.snapPath, snap)

	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe = idx.paths[pathName]
	if pe == nil || pe.persist != p {
		return
	}
	if err != nil {
		_ = p.openJournalAppend()
		return
	}
	_ = p.truncateJournal()
	for i := range pe.internedTracks {
		p.savedCodec[uint8(i+1)] = struct{}{}
	}
}

func (idx *Index) persistCodecsLocked(pe *pathIndex) {
	if pe == nil || pe.persist == nil || !pe.persist.ready {
		return
	}
	for i, tracks := range pe.internedTracks {
		id := uint8(i + 1)
		if _, ok := pe.persist.savedCodec[id]; ok {
			continue
		}
		if err := pe.persist.writeOp(dvrJournalOp{Op: dvrOpCodec, CodecID: id, Tracks: tracks}); err != nil {
			return
		}
		pe.persist.savedCodec[id] = struct{}{}
	}
}

// PersistUpsert writes a completed segment to the per-path journal.
func (idx *Index) PersistUpsert(pathName, fpath string) {
	if pathName == "" || fpath == "" {
		return
	}
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	if !idx.persistOK {
		return
	}
	pe := idx.paths[pathName]
	if pe == nil {
		return
	}
	idx.ensurePersistLocked(pathName, pe)
	if pe.persist == nil || !pe.persist.ready {
		return
	}
	name := filepath.Base(fpath)
	seg, ok := pe.byName[name]
	if !ok || !seg.fmp4.Ready {
		return
	}
	idx.persistCodecsLocked(pe)
	rec := dvrSegRec{
		Rel:      dvrRelPath(pe.commonPath, seg.Fpath),
		Start:    seg.Start,
		Duration: seg.fmp4.Duration,
		Moof:     seg.fmp4.MoofCount,
		CodecID:  internCodecID(pe, seg.fmp4.Tracks),
		Ready:    true,
	}
	_ = pe.persist.writeOp(dvrJournalOp{Op: dvrOpUpsert, Seg: rec})
	if pe.persist.journalOps >= dvrCompactEvery {
		idx.mutex.Unlock()
		idx.compactPath(pathName)
		idx.mutex.Lock()
	}
}

func (idx *Index) persistDeleteLocked(pe *pathIndex, fpath string) {
	if pe == nil || pe.persist == nil || !pe.persist.ready {
		return
	}
	rel := dvrRelPath(pe.commonPath, fpath)
	_ = pe.persist.writeOp(dvrJournalOp{Op: dvrOpDelete, Seg: dvrSegRec{Rel: rel}})
}

func (idx *Index) ensurePersistLocked(pathName string, pe *pathIndex) {
	if pe.persist != nil {
		if !pe.persist.ready {
			_ = pe.persist.openJournalAppend()
		}
		return
	}
	if idx.pathConfs == nil {
		return
	}
	pathConf, _, err := conf.FindPathConf(idx.pathConfs, pathName)
	if err != nil {
		return
	}
	p := newDvrPersist(pathConf, pathName)
	_, _, common := dvrIndexPaths(pathConf, pathName)
	pe.persist = p
	pe.commonPath = common
	_ = p.openJournalAppend()
}

// ClosePersist flushes snapshots and closes journal files.
func (idx *Index) ClosePersist() {
	idx.mutex.Lock()
	names := make([]string, 0, len(idx.paths))
	for name := range idx.paths {
		names = append(names, name)
	}
	idx.mutex.Unlock()
	for _, name := range names {
		idx.compactPath(name)
		idx.mutex.Lock()
		if pe := idx.paths[name]; pe != nil && pe.persist != nil {
			pe.persist.closeJournal()
			pe.persist.ready = false
		}
		idx.mutex.Unlock()
	}
}

// Add inserts or updates a segment.
func (idx *Index) Add(pathName, fpath string, start time.Time) {
	if pathName == "" || fpath == "" || start.IsZero() {
		return
	}

	idx.mutex.Lock()
	defer idx.mutex.Unlock()

	pe := idx.ensurePathLocked(pathName)
	idx.addLocked(pe, fpath, start)
}

// AddFromPath decodes start time from fpath using path config and adds the segment.
func (idx *Index) AddFromPath(pathName, fpath string) {
	start, ok := idx.decodeStart(pathName, fpath)
	if !ok {
		return
	}
	idx.Add(pathName, fpath, start)
}

// CompleteSegment records a closed segment using duration from the recorder.
// The file is not parsed when codec tracks are already interned for the path.
func (idx *Index) CompleteSegment(pathName, fpath string, duration time.Duration) {
	if pathName == "" || fpath == "" {
		return
	}
	idx.AddFromPath(pathName, fpath)

	idx.mutex.RLock()
	pathConfs := idx.pathConfs
	idx.mutex.RUnlock()
	if pathConfs == nil {
		return
	}
	pathConf, _, err := conf.FindPathConf(pathConfs, pathName)
	if err != nil || pathConf.RecordFormat != conf.RecordFormatFMP4 {
		return
	}

	nominal := time.Duration(pathConf.RecordSegmentDuration)
	part := time.Duration(pathConf.RecordPartDuration)
	if duration <= 0 {
		duration = nominal
	}
	tracks := idx.internedTracksOf(pathName)
	meta := fmp4SegMeta{
		Duration:  duration,
		MoofCount: estimateMoofCount(duration, part, nominal),
		Tracks:    tracks,
		Ready:     duration > 0,
	}
	if !meta.Ready {
		return
	}
	if len(tracks) == 0 {
		if ins, ierr := inspectFMP4Segment(fpath); ierr == nil && ins.Ready {
			ins.Duration = duration
			if ins.MoofCount == 0 {
				ins.MoofCount = meta.MoofCount
			}
			meta = ins
		}
	}
	idx.SetFMP4Meta(pathName, fpath, meta)
	idx.PersistUpsert(pathName, fpath)
}

// Remove deletes a segment by absolute file path.
func (idx *Index) Remove(fpath string) {
	if fpath == "" {
		return
	}
	name := filepath.Base(fpath)
	clean := filepath.Clean(fpath)

	idx.mutex.Lock()
	defer idx.mutex.Unlock()

	for _, pe := range idx.paths {
		seg, ok := pe.byName[name]
		if !ok {
			continue
		}
		if seg.Fpath != fpath && filepath.Clean(seg.Fpath) != clean {
			continue
		}
		delete(pe.byName, name)
		for i, s := range pe.segments {
			if s == seg {
				pe.segments = append(pe.segments[:i], pe.segments[i+1:]...)
				break
			}
		}
		pe.rangesOK = false
		fmp4InitSizeCache.Delete(fpath)
		fmp4InitSizeCache.Delete(clean)
		idx.persistDeleteLocked(pe, fpath)
		if pe.persist != nil && pe.persist.journalOps >= dvrCompactEvery {
			pathName := ""
			for n, p := range idx.paths {
				if p == pe {
					pathName = n
					break
				}
			}
			if pathName != "" {
				idx.mutex.Unlock()
				idx.compactPath(pathName)
				idx.mutex.Lock()
			}
		}
		return
	}
}

// Ranges returns cached recording ranges for a path.
func (idx *Index) Ranges(pathName string) []RecordingRange {
	idx.mutex.Lock()
	defer idx.mutex.Unlock()

	pe := idx.paths[pathName]
	if pe == nil || len(pe.segments) == 0 {
		return []RecordingRange{}
	}
	if !pe.rangesOK {
		pe.ranges = buildRanges(pe.timedSegsLocked(time.Now()), pe.segmentDuration, time.Now())
		pe.rangesOK = true
	}
	out := make([]RecordingRange, len(pe.ranges))
	copy(out, pe.ranges)

	// Last fMP4/mpegts segment may still be open: grow the tail to now, but only
	// if it started within one nominal segment (otherwise it is already closed).
	if n := len(pe.segments); n > 0 && !pe.segments[n-1].fmp4.Ready {
		last := pe.segments[n-1]
		maxAge := pe.segmentDuration + rangeMergeTolerance(pe.segmentDuration)
		if maxAge < time.Second {
			maxAge = time.Second
		}
		if time.Since(last.Start) <= maxAge {
			lastStart := last.Start.Unix()
			nowUnix := time.Now().Unix()
			if nowUnix > lastStart {
				if len(out) == 0 {
					out = []RecordingRange{{From: lastStart, Duration: nowUnix - lastStart}}
				} else if out[len(out)-1].From <= lastStart {
					out[len(out)-1].Duration = nowUnix - out[len(out)-1].From
				}
			}
		}
	}
	return out
}

func (pe *pathIndex) timedSegsLocked(now time.Time) []timedSeg {
	out := make([]timedSeg, len(pe.segments))
	for i, s := range pe.segments {
		dur := pe.segmentDuration
		switch {
		case s.fmp4.Ready && s.fmp4.Duration > 0:
			dur = s.fmp4.Duration
		case i+1 < len(pe.segments):
			delta := pe.segments[i+1].Start.Sub(s.Start)
			if delta > 0 && delta <= pe.segmentDuration+rangeMergeTolerance(pe.segmentDuration) {
				dur = delta
			}
		default:
			if age := now.Sub(s.Start); age > 0 && age < pe.segmentDuration {
				dur = age
			}
		}
		out[i] = timedSeg{Start: s.Start, Duration: dur}
	}
	return out
}

// SegmentsInWindow returns segments that overlap [start, start+duration].
// A segment that started before `start` is included if it still covers that instant
// (otherwise archive-{from}-* skips the current file and the player jumps forward).
func (idx *Index) SegmentsInWindow(pathName string, start time.Time, duration time.Duration) []*IndexedSegment {
	if duration > maxArchiveDuration {
		duration = maxArchiveDuration
	}
	if duration <= 0 {
		return nil
	}
	end := start.Add(duration)

	idx.mutex.RLock()
	defer idx.mutex.RUnlock()

	pe := idx.paths[pathName]
	if pe == nil {
		return nil
	}

	i := sort.Search(len(pe.segments), func(i int) bool {
		return !pe.segments[i].Start.Before(start)
	})
	if i > 0 && segmentOverlaps(pe.segments[i-1], start, end, pe.segmentDuration) {
		i--
	}

	var out []*IndexedSegment
	for _, s := range pe.segments[i:] {
		if s.Start.After(end) {
			break
		}
		cp := *s
		out = append(out, &cp)
	}
	return out
}

func segmentOverlaps(s *IndexedSegment, start, end time.Time, nominal time.Duration) bool {
	dur := nominal
	if s.fmp4.Ready && s.fmp4.Duration > 0 {
		dur = s.fmp4.Duration
	}
	if dur <= 0 {
		return false
	}
	segEnd := s.Start.Add(dur)
	return s.Start.Before(end) && segEnd.After(start)
}

// FindNearest returns the segment that best matches timestamp t:
// the last segment with Start <= t, or the first segment after t if none.
func (idx *Index) FindNearest(pathName string, t time.Time) (*IndexedSegment, bool) {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()

	pe := idx.paths[pathName]
	if pe == nil || len(pe.segments) == 0 {
		return nil, false
	}

	segs := pe.segments
	i := sort.Search(len(segs), func(i int) bool {
		return segs[i].Start.After(t)
	})
	if i == 0 {
		// all segments start after t — take the first
		seg := *segs[0]
		return &seg, true
	}
	// last with Start <= t
	seg := *segs[i-1]
	return &seg, true
}

// LatestFMP4Tracks returns init tracks from the newest fMP4 segment.
func (idx *Index) LatestFMP4Tracks(pathName string) []*fmp4.InitTrack {
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	var fpath string
	if pe != nil {
		for i := len(pe.segments) - 1; i >= 0; i-- {
			seg := pe.segments[i]
			if len(seg.fmp4.Tracks) > 0 {
				tracks := seg.fmp4.Tracks
				idx.mutex.RUnlock()
				return tracks
			}
			if seg.Fpath != "" {
				fpath = seg.Fpath
				break
			}
		}
	}
	idx.mutex.RUnlock()
	if fpath == "" {
		return nil
	}

	tracks := loadFMP4Tracks(fpath)
	if len(tracks) == 0 {
		return nil
	}

	idx.mutex.Lock()
	if pe := idx.paths[pathName]; pe != nil {
		tracks = internTracks(pe, tracks)
		if seg, ok := pe.byName[filepath.Base(fpath)]; ok {
			seg.fmp4.Tracks = tracks
		}
	}
	idx.mutex.Unlock()
	return tracks
}

// FindLatest returns the most recent segment for a path.
func (idx *Index) FindLatest(pathName string) (*IndexedSegment, bool) {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()

	pe := idx.paths[pathName]
	if pe == nil || len(pe.segments) == 0 {
		return nil, false
	}
	seg := *pe.segments[len(pe.segments)-1]
	return &seg, true
}

// FindByName returns the absolute path of a segment basename, if present.
func (idx *Index) FindByName(pathName, fileName string) (string, bool) {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()

	pe := idx.paths[pathName]
	if pe == nil {
		return "", false
	}
	seg, ok := pe.byName[fileName]
	if !ok {
		return "", false
	}
	return seg.Fpath, true
}

func (idx *Index) ensurePathLocked(pathName string) *pathIndex {
	pe := idx.paths[pathName]
	if pe != nil {
		return pe
	}
	dur := time.Hour
	if idx.pathConfs != nil {
		if pathConf, _, err := conf.FindPathConf(idx.pathConfs, pathName); err == nil {
			dur = time.Duration(pathConf.RecordSegmentDuration)
		}
	}
	pe = &pathIndex{
		byName:          make(map[string]*IndexedSegment),
		segmentDuration: dur,
	}
	idx.paths[pathName] = pe
	return pe
}

func (idx *Index) decodeStart(pathName, fpath string) (time.Time, bool) {
	idx.mutex.RLock()
	pathConfs := idx.pathConfs
	idx.mutex.RUnlock()

	if pathConfs == nil {
		return time.Time{}, false
	}
	pathConf, _, err := conf.FindPathConf(pathConfs, pathName)
	if err != nil {
		return time.Time{}, false
	}

	recordPath := recordstore.PathAddExtension(
		strings.ReplaceAll(pathConf.RecordPath, "%path", pathName),
		pathConf.RecordFormat,
	)
	recordPath, _ = filepath.Abs(recordPath)
	fpathAbs, _ := filepath.Abs(fpath)

	var pa recordstore.Path
	if !pa.Decode(recordPath, fpathAbs) {
		return time.Time{}, false
	}
	return pa.Start, true
}
