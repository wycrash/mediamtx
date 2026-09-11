package compatapi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

const (
	reconcileWalkBatch    = 20
	reconcileWalkPause    = 20 * time.Millisecond
	reconcileInspectPause = 40 * time.Millisecond
	reconcileEdgeWindow   = 24 * time.Hour
	reconcileReadDirBatch = 512
)

var errReconcileStop = errors.New("dvr index reconcile stopped")

// fmp4SegMeta is cached fMP4 playlist metadata so m3u8 generation does not touch disk.
// Tracks are interned on pathIndex and referenced by codecID (1-based).
type fmp4SegMeta struct {
	Duration  time.Duration
	MoofCount uint32
	codecID   uint8
	Ready     bool
}

// IndexedSegment is a recording segment tracked in memory.
// Rel is relative to common (or an absolute path when common is empty).
type IndexedSegment struct {
	Rel    string
	Start  time.Time
	common string
	fmp4   fmp4SegMeta
	codecs *[][]*fmp4.InitTrack
	// countedInMeta is true once this Ready segment is reflected in day
	// counters and diskRanges. Index/meta are updated incrementally; writeMeta
	// only persists those caches and must not rescan pe.segments.
	countedInMeta bool
}

// Name is the segment basename used in playlists and byName lookups.
func (s *IndexedSegment) Name() string {
	if s == nil || s.Rel == "" {
		return ""
	}
	return filepath.Base(s.Rel)
}

// Fpath is the absolute file path reconstructed from the interned common prefix.
func (s *IndexedSegment) Fpath() string {
	if s == nil {
		return ""
	}
	if s.common == "" || filepath.IsAbs(filepath.FromSlash(s.Rel)) {
		return filepath.FromSlash(s.Rel)
	}
	return dvrAbsPath(s.common, s.Rel)
}

func (s *IndexedSegment) tracks() []*fmp4.InitTrack {
	if s == nil || s.codecs == nil {
		return nil
	}
	return codecTracks(*s.codecs, s.fmp4.codecID)
}

func codecTracks(codecs [][]*fmp4.InitTrack, id uint8) []*fmp4.InitTrack {
	if id == 0 || int(id) > len(codecs) {
		return nil
	}
	return codecs[id-1]
}

// segmentRelFast derives a relative path without filepath.Abs syscalls.
func segmentRelFast(common, fpath string) string {
	if fpath == "" {
		return ""
	}
	if common == "" {
		return filepath.ToSlash(fpath)
	}
	rest, ok := strings.CutPrefix(fpath, common)
	if !ok {
		return dvrRelPath(common, fpath)
	}
	rest = strings.TrimLeft(rest, `/\`)
	if rest == "" {
		return filepath.ToSlash(filepath.Base(fpath))
	}
	return filepath.ToSlash(rest)
}

func bindSeg(pe *pathIndex, fpath string, start time.Time) *IndexedSegment {
	common := ""
	var codecs *[][]*fmp4.InitTrack
	if pe != nil {
		common = pe.commonFor(fpath)
		codecs = &pe.internedTracks
	}
	return &IndexedSegment{
		Rel:    segmentRelFast(common, fpath),
		Start:  start,
		common: common,
		codecs: codecs,
	}
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
	layout          dvrPathLayout
	layouts         []dvrPathLayout
	days            []dvrDayInfo
	diskDays        map[string]map[string]int // common → day → nseg
	diskRanges      map[string][]RecordingRange
	openDay         string
	pinnedDays      map[string]struct{}
	// repairDays are (disk,day) pairs whose journal/snap failed to load.
	// Reconcile rebuilds only these days — not the whole path.
	repairDays []repairDay
	// complete is true after a trusted meta load or a finished directory
	// rebuild. Live OnSegmentComplete inserts must not set this: otherwise a
	// path that is still missing its on-disk index looks non-empty and only
	// the new edge is indexed while other cameras are still rebuilding.
	complete bool
}

// repairDay identifies a per-disk day index that must be rebuilt from media files.
type repairDay struct {
	common string
	day    string
}

func (pe *pathIndex) setLayouts(layouts []dvrPathLayout) {
	if pe == nil {
		return
	}
	pe.layouts = layouts
	if len(layouts) > 0 {
		pe.layout = layouts[0]
		pe.commonPath = layouts[0].common
	}
}

func (pe *pathIndex) allLayouts() []dvrPathLayout {
	if pe == nil {
		return nil
	}
	if len(pe.layouts) > 0 {
		return pe.layouts
	}
	if pe.layout.common != "" || pe.layout.meta != "" {
		return []dvrPathLayout{pe.layout}
	}
	return nil
}

func (pe *pathIndex) commonFor(fpath string) string {
	if pe == nil {
		return ""
	}
	if l := layoutForFpath(pe.allLayouts(), fpath); l.common != "" {
		return l.common
	}
	return pe.commonPath
}

func (pe *pathIndex) selectLayoutForFpath(fpath string) {
	if pe == nil {
		return
	}
	if l := layoutForFpath(pe.allLayouts(), fpath); l.common != "" {
		pe.layout = l
	}
}

func (pe *pathIndex) setDiskDayNSeg(common, day string, n int) {
	if pe == nil || common == "" || day == "" {
		return
	}
	if n < 0 {
		n = 0
	}
	if pe.diskDays == nil {
		pe.diskDays = make(map[string]map[string]int)
	}
	if pe.diskDays[common] == nil {
		pe.diskDays[common] = make(map[string]int)
	}
	pe.diskDays[common][day] = n
}

func (pe *pathIndex) dayNSeg(day string) int {
	if pe == nil || day == "" {
		return 0
	}
	for _, d := range pe.days {
		if d.Date == day {
			return int(d.NSeg)
		}
	}
	return 0
}

func (pe *pathIndex) diskDayNSeg(common, day string) int {
	if pe == nil || common == "" || day == "" || pe.diskDays == nil {
		return 0
	}
	return pe.diskDays[common][day]
}

func (pe *pathIndex) loadDiskDays(common string, days []dvrDayInfo) {
	for _, d := range days {
		pe.setDiskDayNSeg(common, d.Date, int(d.NSeg))
	}
}

func (pe *pathIndex) storeDiskRanges(common string, ranges []RecordingRange) {
	if pe == nil || common == "" {
		return
	}
	if pe.diskRanges == nil {
		pe.diskRanges = make(map[string][]RecordingRange)
	}
	pe.diskRanges[common] = append([]RecordingRange(nil), ranges...)
}

func (pe *pathIndex) appendDiskRange(common string, start time.Time, dur time.Duration) {
	if pe == nil || common == "" {
		pe.appendSegRange(start, dur)
		return
	}
	if pe.diskRanges == nil {
		pe.diskRanges = make(map[string][]RecordingRange)
	}
	pe.diskRanges[common] = appendRecordingRange(pe.diskRanges[common], start, dur, pe.segmentDuration)
	pe.appendSegRange(start, dur)
}

func (idx *Index) fillLayoutsLocked(pe *pathIndex, pathName string) {
	if pe == nil || len(pe.layouts) > 0 || idx.pathConfs == nil {
		return
	}
	pathConf, _, err := conf.FindPathConf(idx.pathConfs, pathName)
	if err != nil || pathConf == nil {
		return
	}
	pe.setLayouts(makeDvrLayouts(pathConf, pathName))
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
	dayCache  map[dayCacheKey]*loadedDay
	dayLRU    []dayCacheKey

	progressMu   sync.RWMutex
	progressPath string

	// snapMemo caches parsed day journals for the duration of loadPath so the
	// same file is not ReadFile'd and replayed for healthy-check, repair, and pin.
	snapMemoMu sync.Mutex
	snapMemo   map[string]snapMemoEntry

	// debug tracks hot-path CPU/disk work (segment churn, writeMeta, reconcile).
	debug indexDebugStats
	// Parent receives slow-op Info logs when set by Server.
	Parent logger.Writer
}

type snapMemoEntry struct {
	snap    dvrSnapshot
	ops     []dvrJournalOp
	ok      bool
	corrupt bool
}

// NewIndex allocates an Index.
func NewIndex() *Index {
	return &Index{
		paths:    make(map[string]*pathIndex),
		dayCache: make(map[dayCacheKey]*loadedDay),
	}
}

func (idx *Index) logInfo(format string, args ...any) {
	if idx == nil || idx.Parent == nil {
		return
	}
	idx.Parent.Log(logger.Info, format, args...)
}

func (idx *Index) logSlow(op, pathName string, d time.Duration, detail string) {
	if idx == nil || idx.Parent == nil || d < slowOpLogThreshold {
		return
	}
	if detail == "" {
		idx.Parent.Log(logger.Info, "slow %s path=%s took %s", op, pathName, d)
		return
	}
	idx.Parent.Log(logger.Info, "slow %s path=%s took %s (%s)", op, pathName, d, detail)
}

// DebugSnapshot returns cumulative hot-path counters since the last ResetDebugInterval.
func (idx *Index) DebugSnapshot() debugStatsSnapshot {
	if idx == nil {
		return debugStatsSnapshot{}
	}
	return idx.debug.snapshot()
}

// ResetDebugInterval returns the interval snapshot and zeroes interval counters.
func (idx *Index) ResetDebugInterval() debugStatsSnapshot {
	if idx == nil {
		return debugStatsSnapshot{}
	}
	return idx.debug.resetInterval()
}

// PendingDayRepairCount returns how many path/day journals still need repair.
func (idx *Index) PendingDayRepairCount() int {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	n := 0
	for _, pe := range idx.paths {
		if pe != nil {
			n += len(pe.repairDays)
		}
	}
	return n
}

// EnablePersist installs path config so live OnSegmentCreate/Complete can
// decode files and append journals before LoadFromDisk finishes.
func (idx *Index) EnablePersist(pathConfs map[string]*conf.Path) {
	idx.mutex.Lock()
	idx.pathConfs = pathConfs
	idx.persistOK = true
	idx.mutex.Unlock()
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
			pe.setLayouts(makeDvrLayouts(pathConf, name))
			if pe.persist != nil {
				pe.persist.hash = dvrIndexHash(pathConf, name)
			}
		}
	}
	idx.mutex.Unlock()

	before := idx.SegmentCount()
	newNames := make([]string, 0)
	for _, pathName := range recordingPathNames(pathConfs) {
		if _, ok := known[pathName]; ok {
			continue
		}
		newNames = append(newNames, pathName)
	}
	for i, pathName := range newNames {
		pathConf, _, err := conf.FindPathConf(pathConfs, pathName)
		if err != nil {
			continue
		}
		st.Paths++
		idx.logInfo("loading recording index path=%s (%d/%d)", pathName, i+1, len(newNames))
		tPath := time.Now()
		ps := idx.loadPath(pathConf, pathName, nil)
		if ps.fromDisk {
			st.DiskPaths++
		}
		idx.logInfo("loaded recording index path=%s (%d/%d) fromDisk=%v segments=%d in %s",
			pathName, i+1, len(newNames), ps.fromDisk, ps.segments, time.Since(tPath))
	}
	st.Segments = idx.SegmentCount() - before
	return st
}

// LoadFromDisk loads snapshot+journal if present. It never walks recordings:
// the HTTP API can start immediately. A missing or broken index is rebuilt
// immediately by ReconcileAll without I/O throttling.
// Live segments already in RAM (OnSegmentCreate during startup) are kept.
func (idx *Index) LoadFromDisk(pathConfs map[string]*conf.Path) IndexLoadStats {
	return idx.loadFromDisk(pathConfs, nil)
}

func (idx *Index) loadFromDisk(pathConfs map[string]*conf.Path, stop <-chan struct{}) IndexLoadStats {
	var st IndexLoadStats
	idx.mutex.Lock()
	idx.pathConfs = pathConfs
	idx.persistOK = true
	idx.mutex.Unlock()

	tScan := time.Now()
	pathNames := recordingPathNames(pathConfs)
	idx.logInfo("scanning recording paths found %d in %s", len(pathNames), time.Since(tScan))
	st.Paths = len(pathNames)
	for i, pathName := range pathNames {
		if stopped(stop) {
			break
		}
		pathConf, _, err := conf.FindPathConf(pathConfs, pathName)
		if err != nil {
			continue
		}
		idx.logInfo("loading recording index path=%s (%d/%d)", pathName, i+1, len(pathNames))
		tPath := time.Now()
		var tr *loadPathOps
		if i < loadPathDetailN {
			tr = &loadPathOps{}
		}
		ps := idx.loadPath(pathConf, pathName, tr)
		if ps.fromDisk {
			st.DiskPaths++
		}
		idx.logInfo("loaded recording index path=%s (%d/%d) fromDisk=%v segments=%d days=%d present=%d missing=%d pin=%d layouts=%d in %s",
			pathName, i+1, len(pathNames), ps.fromDisk, ps.segments, ps.days, ps.present, ps.missing, ps.pinned, ps.layouts, time.Since(tPath))
		idx.logLoadPathOps(pathName, tr)
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
	hasPathVar := strings.Contains(pathConf.RecordPath, "%path")
	for _, raw := range pathConf.RecordPathFormats() {
		recordPath := recordstore.PathAddExtension(raw, pathConf.RecordFormat)
		recordPath, _ = filepath.Abs(recordPath)
		common := recordstore.CommonPath(recordPath)
		if common == "" {
			continue
		}
		entries, err := os.ReadDir(common)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, dvrSnapName) || strings.HasPrefix(name, dvrMetaName) {
				continue
			}
			if isDayDirName(name) {
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
	}
	return ret
}

type pathLoadStats struct {
	fromDisk bool
	segments int
	days     int
	present  int
	missing  int
	pinned   int
	layouts  int
}

const loadPathDetailN = 2

type loadDayOp struct {
	day      string
	d        time.Duration
	read     time.Duration
	parse    time.Duration
	segs     int
	ops      int
	snapB    int64
	journalB int64
	ok       bool
	corrupt  bool
}

type loadPathOps struct {
	layouts time.Duration
	meta    time.Duration
	days    time.Duration
	pin     time.Duration
	bind    time.Duration
	ranges  time.Duration
	merge   time.Duration
	pinN    int
	rebuild bool
	dayOps  []loadDayOp
}

func (op *loadPathOps) summary() string {
	if op == nil {
		return ""
	}
	okN := 0
	for _, d := range op.dayOps {
		if d.ok {
			okN++
		}
	}
	ranges := op.ranges.String()
	if op.rebuild {
		ranges = "rebuild " + ranges
	}
	return fmt.Sprintf("layouts=%s meta=%s days=%s (n=%d ok=%d) pin=%s (n=%d) bind=%s ranges=%s merge=%s",
		op.layouts, op.meta, op.days, len(op.dayOps), okN, op.pin, op.pinN, op.bind, ranges, op.merge)
}

func (op *loadPathOps) daysLine() string {
	if op == nil || len(op.dayOps) == 0 {
		return ""
	}
	days := append([]loadDayOp(nil), op.dayOps...)
	sort.Slice(days, func(i, j int) bool { return days[i].d > days[j].d })
	limit := len(days)
	prefix := ""
	if limit > 6 {
		limit = 3
		prefix = "slowest "
	}
	parts := make([]string, 0, limit)
	for _, d := range days[:limit] {
		st := "ok"
		if d.corrupt {
			st = "corrupt"
		} else if !d.ok {
			st = "skip"
		}
		parts = append(parts, fmt.Sprintf("%s %s read=%s parse=%s segs=%d ops=%d snap=%s journal=%s %s",
			d.day, d.d, d.read, d.parse, d.segs, d.ops, formatLoadBytes(d.snapB), formatLoadBytes(d.journalB), st))
	}
	s := prefix + strings.Join(parts, "; ")
	if len(days) > limit {
		s += fmt.Sprintf(" (%d more)", len(days)-limit)
	}
	return s
}

func formatLoadBytes(n int64) string {
	if n <= 0 {
		return "0B"
	}
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
}

func (idx *Index) logLoadPathOps(pathName string, tr *loadPathOps) {
	if tr == nil {
		return
	}
	idx.logInfo("recording index path=%s load ops %s", pathName, tr.summary())
	if line := tr.daysLine(); line != "" {
		idx.logInfo("recording index path=%s load days %s", pathName, line)
	}
}

func (idx *Index) loadPath(pathConf *conf.Path, pathName string, tr *loadPathOps) pathLoadStats {
	idx.beginSnapMemo()
	defer idx.endSnapMemo()

	var st pathLoadStats
	tLayouts := time.Now()
	layouts := makeDvrLayouts(pathConf, pathName)
	if tr != nil {
		tr.layouts = time.Since(tLayouts)
	}
	st.layouts = len(layouts)

	p := newDvrPersist(pathConf, pathName)
	nominal := time.Duration(pathConf.RecordSegmentDuration)
	today := dvrDayDate(time.Now())

	var mergedRanges []RecordingRange
	var mergedDays []dvrDayInfo
	var repairs []repairDay
	trusted := 0

	for _, l := range layouts {
		meta, diskRepairs, loadedDays, ok := idx.loadDiskMeta(l, p.hash, tr)
		if !ok {
			continue
		}
		trusted++
		repairs = append(repairs, diskRepairs...)
		mergedRanges = mergeRecordingRanges(mergedRanges, meta.Ranges, nominal)
		mergedDays = mergeDayInfosSum(mergedDays, loadedDays)
		idx.mutex.Lock()
		pe := idx.ensurePathLocked(pathName)
		pe.loadDiskDays(l.common, loadedDays)
		pe.storeDiskRanges(l.common, meta.Ranges)
		idx.mutex.Unlock()
		st.present += len(loadedDays)
		st.missing += len(diskRepairs)
	}

	idx.mutex.Lock()
	pe := idx.ensurePathLocked(pathName)
	pe.setLayouts(layouts)
	if pe.persist == nil {
		pe.persist = p
	}
	idx.mutex.Unlock()

	if trusted == 0 {
		tMerge := time.Now()
		idx.mergeLiveAfterLoad(pathName)
		if tr != nil {
			tr.merge = time.Since(tMerge)
		}
		return st
	}

	rangesOK := len(mergedRanges) > 0 || len(mergedDays) == 0
	idx.mutex.Lock()
	pe = idx.ensurePathLocked(pathName)
	pe.setLayouts(layouts)
	if pe.persist == nil {
		pe.persist = p
	}
	pe.ranges = mergedRanges
	pe.rangesOK = rangesOK
	pe.days = mergedDays
	pe.repairDays = repairs
	pe.complete = trusted == len(layouts)
	livePinned := pe.pinnedDays
	liveOpen := pe.openDay
	pe.pinnedDays = make(map[string]struct{})
	for d := range livePinned {
		pe.pinnedDays[d] = struct{}{}
	}
	if liveOpen != "" {
		pe.openDay = liveOpen
	}
	last := pe.lastDay()
	idx.mutex.Unlock()

	st.days = len(mergedDays)
	hotStart := time.Now().Add(-reconcileEdgeWindow)
	hotEnd := time.Now().Add(time.Second)
	tPin := time.Now()
	for _, d := range mergedDays {
		if d.Date == today || d.Date == last || dayOverlapsWindow(d.Date, hotStart, hotEnd) {
			idx.pinDay(pathName, d.Date)
			st.pinned++
		}
	}
	if tr != nil {
		tr.pin = time.Since(tPin)
		tr.pinN = st.pinned
	}
	tBind := time.Now()
	for _, d := range mergedDays {
		if d.Date != today {
			continue
		}
		idx.mutex.RLock()
		alreadyBound := false
		if pe := idx.paths[pathName]; pe != nil && pe.persist != nil && pe.persist.ready && pe.openDay == today {
			alreadyBound = true
		}
		idx.mutex.RUnlock()
		if !alreadyBound {
			idx.bindPersist(pathName, today)
		}
		journalOps := 0
		for _, l := range layouts {
			if ops, ok := idx.cachedJournalOps(l, today); ok {
				journalOps += len(ops)
			}
		}
		idx.mutex.Lock()
		if pe := idx.paths[pathName]; pe != nil && pe.persist != nil && pe.persist.journal == nil {
			pe.persist.journalOps = journalOps
		}
		idx.mutex.Unlock()
		break
	}
	if tr != nil {
		tr.bind = time.Since(tBind)
	}
	tRanges := time.Now()
	if !rangesOK || !rangesCoverDays(mergedRanges, mergedDays) {
		idx.rebuildRangesFromDayFiles(pathName, false)
		if tr != nil {
			tr.rebuild = true
		}
	}
	if tr != nil {
		tr.ranges = time.Since(tRanges)
	}
	tMerge := time.Now()
	idx.mergeLiveAfterLoad(pathName)
	if tr != nil {
		tr.merge = time.Since(tMerge)
	}
	st.fromDisk = trusted == len(layouts)
	st.segments = idx.pathSegmentCount(pathName)
	return st
}

// mergeLiveAfterLoad re-applies in-RAM live segments onto ranges/day counts
// after a disk load overwrote them from meta.
func (idx *Index) mergeLiveAfterLoad(pathName string) {
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe := idx.paths[pathName]
	if pe == nil || len(pe.segments) == 0 {
		return
	}
	dayN := make(map[string]int)
	for _, s := range pe.segments {
		if s == nil {
			continue
		}
		dur := s.fmp4.Duration
		if dur <= 0 {
			dur = pe.segmentDuration
		}
		if dur > 0 {
			pe.appendSegRange(s.Start, dur)
		}
		dayN[dvrDayDate(s.Start)]++
	}
	for day, n := range dayN {
		if n > pe.dayNSeg(day) {
			pe.setDayNSeg(day, n)
		}
	}
}

// ReconcileAll repairs the index without re-reading known files.
// Incomplete paths (missing/corrupt meta) get a full directory rebuild.
// Paths with trusted meta but damaged day journals rebuild only those days.
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

	tScan := time.Now()
	pathNames := recordingPathNames(pathConfs)
	if !slow {
		idx.logInfo("recording index scanning paths found %d in %s", len(pathNames), time.Since(tScan))
	}
	st.Paths = len(pathNames)
	nPaths := len(pathNames)
	for i, pathName := range pathNames {
		if stopped(stop) {
			break
		}
		idx.setProgressPath(pathName)
		pathConf, _, err := conf.FindPathConf(pathConfs, pathName)
		if err != nil {
			continue
		}
		if !slow {
			idx.logInfo("recording index updating path=%s (%d/%d)", pathName, i+1, nPaths)
		}
		tPath := time.Now()
		var ins, add, del int
		mode := "edges"
		if idx.pathNeedsRebuild(pathName) {
			mode = "rebuild"
			ins, add, del = idx.buildPathFromDir(pathName, pathConf, stop, slow)
			if add > 0 {
				st.Built++
			}
		} else {
			if repairs := idx.takeRepairDays(pathName); len(repairs) > 0 {
				mode = "repairDays"
				ins, add, del = idx.rebuildRepairDays(pathName, pathConf, repairs, stop, slow)
				if add > 0 {
					st.Built++
				}
			}
			if !slow && idx.rangesNeedRepair(pathName) {
				mode = "ranges+edges"
				idx.rebuildRangesFromDayFiles(pathName, true)
			}
			i2, a2, d2 := idx.reconcilePathEdges(pathName, pathConf, stop, slow)
			ins += i2
			add += a2
			del += d2
		}
		pathDur := time.Since(tPath)
		idx.debug.noteReconcilePath(pathDur)
		if !slow {
			idx.logInfo("recording index updated path=%s (%d/%d) mode=%s ins=%d add=%d del=%d in %s",
				pathName, i+1, nPaths, mode, ins, add, del, pathDur)
		} else {
			idx.logSlow("reconcilePath", pathName, pathDur,
				fmt.Sprintf("mode=%s ins=%d add=%d del=%d", mode, ins, add, del))
		}
		st.Inspected += ins
		st.Added += add
		st.Removed += del
	}
	idx.setProgressPath("")
	st.Segments = idx.SegmentCount()
	st.DiskPaths = st.Paths
	return st
}

func (idx *Index) takeRepairDays(pathName string) []repairDay {
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe := idx.paths[pathName]
	if pe == nil || len(pe.repairDays) == 0 {
		return nil
	}
	out := pe.repairDays
	pe.repairDays = nil
	return out
}

// HasPendingDayRepairs reports whether any path still has damaged day indexes.
func (idx *Index) HasPendingDayRepairs() bool {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	for _, pe := range idx.paths {
		if pe != nil && len(pe.repairDays) > 0 {
			return true
		}
	}
	return false
}

func (idx *Index) pathNeedsRebuild(pathName string) bool {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	pe := idx.paths[pathName]
	return pe == nil || !pe.complete
}

// MarkNeedsRebuild forces the next ReconcileAll to rebuild pathName from disk.
func (idx *Index) MarkNeedsRebuild(pathName string) {
	if pathName == "" {
		return
	}
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe := idx.ensurePathLocked(pathName)
	pe.complete = false
	pe.repairDays = nil
}

// MarkAllNeedsRebuild forces rebuild of every known path. Returns how many were marked.
func (idx *Index) MarkAllNeedsRebuild() int {
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	n := 0
	for _, pe := range idx.paths {
		pe.complete = false
		n++
	}
	return n
}

// NeedsRebuildCount returns how many loaded paths are marked incomplete.
func (idx *Index) NeedsRebuildCount() int {
	return len(idx.NeedsRebuildPaths())
}

// NeedsRebuildPaths returns sorted path names marked incomplete.
func (idx *Index) NeedsRebuildPaths() []string {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	out := make([]string, 0)
	for name, pe := range idx.paths {
		if !pe.complete {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (idx *Index) setProgressPath(pathName string) {
	idx.progressMu.Lock()
	idx.progressPath = pathName
	idx.progressMu.Unlock()
}

// ProgressPath returns the path currently being reconciled, if any.
func (idx *Index) ProgressPath() string {
	idx.progressMu.RLock()
	defer idx.progressMu.RUnlock()
	return idx.progressPath
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
	meta := fmp4SegMeta{Duration: rec.Duration, MoofCount: rec.Moof, codecID: rec.CodecID, Ready: rec.Ready}
	if !meta.Ready && (meta.Duration > 0 || meta.MoofCount > 0 || meta.codecID > 0) {
		meta.Ready = true
	}
	seg.fmp4 = meta
	seg.codecs = &pe.internedTracks
	if meta.Ready {
		// Already accounted in meta/day journals loaded from disk.
		seg.countedInMeta = true
	}
	pe.rangesOK = false
}

func (idx *Index) buildPathFromDir(
	pathName string,
	pathConf *conf.Path,
	stop <-chan struct{},
	slow bool,
) (inspected, added, removed int) {
	layouts := makeDvrLayouts(pathConf, pathName)
	if len(layouts) == 0 {
		return 0, 0, 0
	}
	formats := pathConf.RecordPathFormats()
	nominal := time.Duration(pathConf.RecordSegmentDuration)
	part := time.Duration(pathConf.RecordPartDuration)

	idx.mutex.Lock()
	pe := idx.ensurePathLocked(pathName)
	if pe.persist == nil {
		pe.persist = newDvrPersist(pathConf, pathName)
	}
	pe.setLayouts(layouts)
	pe.days = nil
	pe.diskDays = nil
	pe.diskRanges = nil
	pe.ranges = nil
	pe.rangesOK = false
	pe.complete = false
	pe.repairDays = nil
	pe.segments = nil
	pe.byName = make(map[string]*IndexedSegment)
	pe.pinnedDays = make(map[string]struct{})
	pe.internedTracks = nil
	for k := range idx.dayCache {
		if k.path == pathName {
			delete(idx.dayCache, k)
		}
	}
	hash := pe.persist.hash
	idx.mutex.Unlock()

	type recFile struct {
		fpath string
		start time.Time
	}

	finished := true
	nWalk := 0

	for i, layout := range layouts {
		if layout.common == "" {
			continue
		}
		walkRoot := layout.walkRoot(pathName)
		if walkRoot == "" {
			continue
		}
		layout.removeLegacyMonolith()
		raw := pathConf.RecordPath
		if i < len(formats) {
			raw = formats[i]
		}
		recordPath := recordstore.PathAddExtension(
			strings.ReplaceAll(raw, "%path", pathName),
			pathConf.RecordFormat,
		)
		recordPath, _ = filepath.Abs(recordPath)

		var curDay string
		var cur []recFile

		flush := func(day string, files []recFile) int {
			if day == "" || len(files) == 0 {
				return 0
			}
			sort.Slice(files, func(i, j int) bool { return files[i].start.Before(files[j].start) })
			segs := make([]*IndexedSegment, 0, len(files))
			var codecs [][]*fmp4.InitTrack
			newest := files[len(files)-1].fpath
			var tracks []*fmp4.InitTrack
			var inspectedMeta fmp4SegMeta
			if pathConf.RecordFormat == conf.RecordFormatFMP4 && newest != "" {
				if meta, tr, err := inspectFMP4Segment(newest); err == nil && meta.Ready {
					tracks = internTracksInto(&codecs, tr)
					inspectedMeta = meta
					inspected++
				}
			}
			for _, f := range files {
				seg := &IndexedSegment{
					Rel:    segmentRelFast(layout.common, f.fpath),
					Start:  f.start,
					common: layout.common,
				}
				segs = append(segs, seg)
			}
			if inspectedMeta.Ready {
				last := segs[len(segs)-1]
				last.fmp4 = inspectedMeta
				last.fmp4.codecID = codecIDFrom(codecs, tracks)
			}
			fillSegsMeta(segs, tracks, codecs, nominal, part)
			snap := snapshotFromSegs(hash, layout.common, segs, codecs)
			journalPath := layout.dayJournal(day)
			if err := writeDayJournalFile(journalPath, hash, snap); err != nil {
				return 0
			}
			// Drop legacy MTXI snapshot so load uses journal as SoT.
			_ = os.Remove(layout.daySnap(day))
			idx.mutex.Lock()
			pe := idx.paths[pathName]
			if pe != nil {
				curN := 0
				for _, d := range pe.days {
					if d.Date == day {
						curN = int(d.NSeg)
						break
					}
				}
				pe.setDayNSeg(day, curN+len(snap.Segs))
				pe.setDiskDayNSeg(layout.common, day, len(snap.Segs))
				for _, seg := range segs {
					pe.appendDiskRange(layout.common, seg.Start, trustedSegDuration(seg.fmp4.Duration, 0, nominal))
				}
			}
			idx.mutex.Unlock()
			return len(files)
		}

		walkErr := filepath.WalkDir(walkRoot, func(fpath string, info fs.DirEntry, err error) error {
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
			if info.IsDir() || isDvrIndexFile(info.Name()) {
				return nil
			}
			var pa recordstore.Path
			if !pa.Decode(recordPath, fpath) {
				return nil
			}
			day := dvrDayDate(pa.Start)
			if curDay != "" && day != curDay {
				added += flush(curDay, cur)
				cur = cur[:0]
			}
			curDay = day
			cur = append(cur, recFile{fpath: fpath, start: pa.Start})
			return nil
		})
		if errors.Is(walkErr, errReconcileStop) {
			finished = false
			break
		}
		if walkErr != nil && !os.IsNotExist(walkErr) {
			return inspected, added, 0
		}
		if curDay != "" {
			added += flush(curDay, cur)
		}
	}
	if added == 0 {
		// Still mark complete when the walk finished, otherwise a forced
		// rebuild of an empty path would loop forever on the worker.
		if finished {
			idx.mutex.Lock()
			if pe := idx.paths[pathName]; pe != nil {
				pe.complete = true
				pe.rangesOK = true
			}
			idx.mutex.Unlock()
		}
		return inspected, 0, 0
	}

	idx.mutex.Lock()
	last := ""
	if pe := idx.paths[pathName]; pe != nil {
		last = pe.lastDay()
	}
	idx.mutex.Unlock()
	today := dvrDayDate(time.Now())
	hotStart := time.Now().Add(-reconcileEdgeWindow)
	idx.mutex.RLock()
	var toPin []string
	if pe := idx.paths[pathName]; pe != nil {
		for _, d := range pe.days {
			if d.Date == today || d.Date == last || dayOverlapsWindow(d.Date, hotStart, time.Now().Add(time.Second)) {
				toPin = append(toPin, d.Date)
			}
		}
	}
	idx.mutex.RUnlock()
	for _, day := range toPin {
		idx.pinDay(pathName, day)
	}
	if today != "" {
		hasToday := false
		idx.mutex.RLock()
		if pe := idx.paths[pathName]; pe != nil {
			for _, d := range pe.days {
				if d.Date == today {
					hasToday = true
					break
				}
			}
		}
		idx.mutex.RUnlock()
		if hasToday {
			idx.bindPersist(pathName, today)
		}
	}
	idx.rebuildRangesFromDayFiles(pathName, true)
	if finished {
		idx.mutex.Lock()
		if pe := idx.paths[pathName]; pe != nil {
			pe.complete = true
			pe.rangesOK = true
		}
		idx.mutex.Unlock()
	}
	return inspected, added, 0
}

// rebuildRepairDays rebuilds only the listed (disk, day) indexes from media files.
func (idx *Index) rebuildRepairDays(
	pathName string,
	pathConf *conf.Path,
	repairs []repairDay,
	stop <-chan struct{},
	slow bool,
) (inspected, added, removed int) {
	if len(repairs) == 0 {
		return 0, 0, 0
	}
	layouts := makeDvrLayouts(pathConf, pathName)
	if len(layouts) == 0 {
		return 0, 0, 0
	}
	formats := pathConf.RecordPathFormats()
	nominal := time.Duration(pathConf.RecordSegmentDuration)
	part := time.Duration(pathConf.RecordPartDuration)

	idx.mutex.Lock()
	pe := idx.ensurePathLocked(pathName)
	if pe.persist == nil {
		pe.persist = newDvrPersist(pathConf, pathName)
	}
	pe.setLayouts(layouts)
	hash := pe.persist.hash
	idx.mutex.Unlock()

	type recFile struct {
		fpath string
		start time.Time
	}

	nWalk := 0
	for ri, rep := range repairs {
		if stopped(stop) {
			idx.mutex.Lock()
			if pe := idx.paths[pathName]; pe != nil {
				pe.repairDays = append(pe.repairDays, repairs[ri:]...)
			}
			idx.mutex.Unlock()
			return inspected, added, removed
		}
		var layout dvrPathLayout
		layoutIdx := -1
		for i, l := range layouts {
			if l.common == rep.common {
				layout = l
				layoutIdx = i
				break
			}
		}
		if layout.common == "" {
			continue
		}
		raw := pathConf.RecordPath
		if layoutIdx >= 0 && layoutIdx < len(formats) {
			raw = formats[layoutIdx]
		}
		recordPath := recordstore.PathAddExtension(
			strings.ReplaceAll(raw, "%path", pathName),
			pathConf.RecordFormat,
		)
		recordPath, _ = filepath.Abs(recordPath)
		walkRoot := layout.walkRoot(pathName)
		if layout.dateDir {
			dayRoot := filepath.Join(walkRoot, rep.day)
			if info, err := os.Stat(dayRoot); err == nil && info.IsDir() {
				walkRoot = dayRoot
			}
		}
		var files []recFile
		walkErr := filepath.WalkDir(walkRoot, func(fpath string, info fs.DirEntry, err error) error {
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
			if info.IsDir() {
				if layout.dateDir && info.Name() != rep.day && isDayDirName(info.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if isDvrIndexFile(info.Name()) {
				return nil
			}
			var pa recordstore.Path
			if !pa.Decode(recordPath, fpath) {
				return nil
			}
			if dvrDayDate(pa.Start) != rep.day {
				return nil
			}
			files = append(files, recFile{fpath: fpath, start: pa.Start})
			return nil
		})
		if errors.Is(walkErr, errReconcileStop) || stopped(stop) {
			idx.mutex.Lock()
			if pe := idx.paths[pathName]; pe != nil {
				pe.repairDays = append(pe.repairDays, repairs[ri:]...)
			}
			idx.mutex.Unlock()
			return inspected, added, removed
		}
		if len(files) == 0 {
			_ = os.Remove(layout.dayJournal(rep.day))
			_ = os.Remove(layout.daySnap(rep.day))
			idx.mutex.Lock()
			if pe := idx.paths[pathName]; pe != nil {
				pe.setDiskDayNSeg(layout.common, rep.day, 0)
				n := 0
				for _, l := range pe.allLayouts() {
					if pe.diskDays[l.common] != nil {
						n += pe.diskDays[l.common][rep.day]
					}
				}
				if n == 0 {
					pe.removeDay(rep.day)
				}
				delete(idx.dayCache, dayCacheKey{path: pathName, day: rep.day})
			}
			idx.mutex.Unlock()
			continue
		}
		sort.Slice(files, func(i, j int) bool { return files[i].start.Before(files[j].start) })
		segs := make([]*IndexedSegment, 0, len(files))
		var codecs [][]*fmp4.InitTrack
		newest := files[len(files)-1].fpath
		var tracks []*fmp4.InitTrack
		var inspectedMeta fmp4SegMeta
		if pathConf.RecordFormat == conf.RecordFormatFMP4 && newest != "" {
			if meta, tr, err := inspectFMP4Segment(newest); err == nil && meta.Ready {
				tracks = internTracksInto(&codecs, tr)
				inspectedMeta = meta
				inspected++
			}
		}
		for _, f := range files {
			segs = append(segs, &IndexedSegment{
				Rel:    segmentRelFast(layout.common, f.fpath),
				Start:  f.start,
				common: layout.common,
			})
		}
		if inspectedMeta.Ready {
			last := segs[len(segs)-1]
			last.fmp4 = inspectedMeta
			last.fmp4.codecID = codecIDFrom(codecs, tracks)
		}
		fillSegsMeta(segs, tracks, codecs, nominal, part)
		snap := snapshotFromSegs(hash, layout.common, segs, codecs)
		if err := writeDayJournalFile(layout.dayJournal(rep.day), hash, snap); err != nil {
			idx.mutex.Lock()
			if pe := idx.paths[pathName]; pe != nil {
				pe.repairDays = append(pe.repairDays, rep)
			}
			idx.mutex.Unlock()
			continue
		}
		_ = os.Remove(layout.daySnap(rep.day))
		added += len(files)
		needPin := false
		idx.mutex.Lock()
		if pe := idx.paths[pathName]; pe != nil {
			pe.setDiskDayNSeg(layout.common, rep.day, len(snap.Segs))
			total := 0
			for _, l := range pe.allLayouts() {
				if pe.diskDays[l.common] != nil {
					total += pe.diskDays[l.common][rep.day]
				}
			}
			pe.setDayNSeg(rep.day, total)
			for _, seg := range segs {
				pe.appendDiskRange(layout.common, seg.Start, trustedSegDuration(seg.fmp4.Duration, 0, nominal))
			}
			delete(idx.dayCache, dayCacheKey{path: pathName, day: rep.day})
			needPin = pe.dayIsPinned(rep.day) || rep.day == pe.openDay || rep.day == dvrDayDate(time.Now())
		}
		idx.mutex.Unlock()
		if needPin {
			idx.pinDay(pathName, rep.day)
		}
	}
	idx.rebuildRangesFromDayFiles(pathName, true)
	idx.writeMeta(pathName)
	return inspected, added, removed
}

func (idx *Index) reconcilePathEdges(
	pathName string,
	pathConf *conf.Path,
	stop <-chan struct{},
	slow bool,
) (inspected, added, removed int) {
	idx.mutex.Lock()
	pe := idx.paths[pathName]
	if pe == nil {
		idx.mutex.Unlock()
		return 0, 0, 0
	}
	if !pe.complete && len(pe.segments) == 0 {
		idx.mutex.Unlock()
		return 0, 0, 0
	}
	idx.ensurePersistLocked(pathName, pe)
	nominal := pe.segmentDuration
	if nominal <= 0 {
		nominal = time.Duration(pathConf.RecordSegmentDuration)
	}
	if nominal <= 0 {
		nominal = time.Hour
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
	idx.evictStalePinned(pathName)
	idx.compactPathIfDirty(pathName, added > 0 || inspected > 0 || removed > 0)
	return inspected, added, removed
}

func (idx *Index) pruneExpired(pathName string, deleteAfter time.Duration) int {
	if deleteAfter <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-deleteAfter)
	idx.mutex.Lock()
	pe := idx.paths[pathName]
	if pe == nil {
		idx.mutex.Unlock()
		return 0
	}
	removed := 0
	var dropDays []string
	for len(pe.days) > 0 {
		d := pe.days[0]
		t, err := time.ParseInLocation("2006-01-02", d.Date, time.Local)
		if err != nil {
			break
		}
		if t.Add(24 * time.Hour).After(cutoff) {
			break
		}
		dropDays = append(dropDays, d.Date)
		removed += int(d.NSeg)
		pe.days = pe.days[1:]
	}
	pe.ranges = trimRangesBefore(pe.ranges, cutoff)
	pe.rangesOK = true
	layouts := append([]dvrPathLayout(nil), pe.allLayouts()...)
	for len(pe.segments) > 0 {
		seg := pe.segments[0]
		if !seg.Start.Before(cutoff) {
			break
		}
		idx.persistDeleteLocked(pe, seg.Fpath(), dvrDayDate(seg.Start))
		idx.removeRelLocked(pe, seg.Rel)
		removed++
	}
	idx.mutex.Unlock()
	for _, day := range dropDays {
		for _, layout := range layouts {
			_ = os.Remove(layout.daySnap(day))
			_ = os.Remove(layout.dayJournal(day))
			if layout.dateDir {
				_ = os.Remove(filepath.Join(layout.common, day))
			}
		}
		idx.mutex.Lock()
		if pe := idx.paths[pathName]; pe != nil {
			idx.unpinDayLocked(pe, day)
			delete(idx.dayCache, dayCacheKey{path: pathName, day: day})
		}
		idx.mutex.Unlock()
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
		fpath := seg.Fpath()
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
			day := ""
			if seg, ok := pe.byName[filepath.Base(fpath)]; ok {
				day = dvrDayDate(seg.Start)
			}
			idx.persistDeleteLocked(pe, fpath, day)
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
		lastPath := last.Fpath()
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
			day := dvrDayDate(lastStart)
			idx.persistDeleteLocked(pe, lastPath, day)
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

	added += idx.adoptWindowsFromDirs(pathName, pathConf, windows, nominal, part, stop, slow)
	return added, inspected, removed
}

// adoptWindowsFromDirs picks up segments the index does not know yet, by
// listing directories: one readdir per disk and day instead of a stat per
// possible segment start. Probing by timestamp cost 17280 stats per camera for
// a 24 h window of 5 s segments, which is what made startup on dozens of
// cameras take minutes of pure metadata I/O. It also could not find much:
// recordPath must contain %f, and real starts are cut on keyframes, so a
// timestamp stepped by segment duration almost never matches a file name.
func (idx *Index) adoptWindowsFromDirs(
	pathName string,
	pathConf *conf.Path,
	windows [][2]time.Time,
	nominal, part time.Duration,
	stop <-chan struct{},
	slow bool,
) int {
	if len(windows) == 0 || pathConf == nil {
		return 0
	}
	ext := ".mp4"
	if pathConf.RecordFormat == conf.RecordFormatMPEGTS {
		ext = ".ts"
	}

	days := windowDays(windows)
	added := 0
	formats := pathConf.RecordPathFormats()
	for i, layout := range makeDvrLayouts(pathConf, pathName) {
		root := layout.walkRoot(pathName)
		if layout.common == "" || root == "" {
			continue
		}
		raw := pathConf.RecordPath
		if i < len(formats) {
			raw = formats[i]
		}
		format := recordstore.PathAddExtension(
			strings.ReplaceAll(raw, "%path", pathName),
			pathConf.RecordFormat,
		)
		format, _ = filepath.Abs(format)

		sc := edgeScan{
			pathName: pathName,
			pathConf: pathConf,
			format:   format,
			ext:      ext,
			windows:  windows,
			nominal:  nominal,
			part:     part,
		}

		if layout.dateDir {
			for _, day := range days {
				if stopped(stop) {
					return added
				}
				added += idx.adoptDirSegments(sc, filepath.Join(root, dvrDayDate(day)), stop, slow)
			}
			continue
		}
		// One flat directory holds the whole retention window, so narrow by
		// name before paying for a decode. A template whose basename does not
		// start with the date yields no prefix for some day, and then the
		// whole filter has to be dropped rather than applied partially.
		for _, day := range days {
			p := dayNamePrefix(format, day)
			if p == "" {
				sc.prefixes = nil
				break
			}
			sc.prefixes = append(sc.prefixes, p)
		}
		added += idx.adoptDirSegments(sc, root, stop, slow)
	}
	return added
}

type edgeScan struct {
	pathName string
	pathConf *conf.Path
	format   string
	ext      string
	// prefixes limits which basenames are worth decoding. Empty accepts all.
	prefixes []string
	windows  [][2]time.Time
	nominal  time.Duration
	part     time.Duration
}

// windowDays returns local midnights of every day the windows touch.
func windowDays(windows [][2]time.Time) []time.Time {
	seen := make(map[string]struct{})
	var out []time.Time
	for _, w := range windows {
		if w[0].IsZero() || w[1].IsZero() || w[0].After(w[1]) {
			continue
		}
		from := w[0].In(time.Local)
		day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.Local)
		for ; !day.After(w[1]) && len(out) <= 32; day = day.AddDate(0, 0, 1) {
			date := dvrDayDate(day)
			if _, ok := seen[date]; ok {
				continue
			}
			seen[date] = struct{}{}
			out = append(out, day)
		}
	}
	return out
}

// dayNamePrefix is the basename prefix every segment recorded on day shares.
// Found by encoding the first and last instant of the day and keeping the
// common head, so it works for any template including %s.
func dayNamePrefix(format string, day time.Time) string {
	lo := filepath.Base(recordstore.Path{Start: day}.Encode(format))
	hi := filepath.Base(recordstore.Path{
		Start: day.AddDate(0, 0, 1).Add(-time.Microsecond),
	}.Encode(format))
	n := 0
	for n < len(lo) && n < len(hi) && lo[n] == hi[n] {
		n++
	}
	return lo[:n]
}

func (idx *Index) adoptDirSegments(sc edgeScan, dir string, stop <-chan struct{}, slow bool) int {
	f, err := os.Open(dir)
	if err != nil {
		return 0
	}
	defer f.Close()

	added, work := 0, 0
	for {
		// Streamed rather than os.ReadDir: a flat camera directory holds the
		// whole retention window and must not be sorted into memory to look at
		// a 24 h edge. Throttling counts adoptions, not directory entries:
		// pausing per entry would take hours on such a directory.
		ents, readErr := f.ReadDir(reconcileReadDirBatch)
		for _, e := range ents {
			if stopped(stop) {
				return added
			}
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, sc.ext) || !nameHasPrefix(name, sc.prefixes) {
				continue
			}
			fpath := filepath.Join(dir, name)
			var pa recordstore.Path
			if !pa.Decode(sc.format, fpath) || !timeInWindows(pa.Start, sc.windows) {
				continue
			}
			if _, ok := idx.FindByName(sc.pathName, name); ok {
				continue
			}
			work++
			if slow && work%reconcileWalkBatch == 0 && !sleepOrStop(stop, reconcileWalkPause) {
				return added
			}
			if idx.adoptDiskSegment(sc.pathName, sc.pathConf, fpath, pa.Start, sc.nominal, sc.part) {
				added++
			}
		}
		if readErr != nil || len(ents) == 0 {
			return added
		}
	}
}

func nameHasPrefix(name string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func timeInWindows(t time.Time, windows [][2]time.Time) bool {
	for _, w := range windows {
		if !t.Before(w[0]) && !t.After(w[1]) {
			return true
		}
	}
	return false
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
		Ready:     true,
	}
	if pathConf.RecordFormat == conf.RecordFormatFMP4 && len(tracks) == 0 {
		if ins, tr, err := inspectFMP4Segment(fpath); err == nil && ins.Ready {
			meta = ins
			tracks = tr
		}
	}
	idx.SetFMP4Meta(pathName, fpath, meta, tracks)
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
	cap := segDurationCap(nominal)
	for i, seg := range pe.segments {
		var nextDelta time.Duration
		if i+1 < len(pe.segments) {
			nextDelta = pe.segments[i+1].Start.Sub(seg.Start)
		}
		dur := trustedSegDuration(seg.fmp4.Duration, nextDelta, nominal)
		if !seg.fmp4.Ready || seg.fmp4.Duration == 0 || seg.fmp4.Duration > cap || (seg.fmp4.codecID == 0 && tracks != nil) {
			if seg.fmp4.MoofCount == 0 {
				seg.fmp4.MoofCount = estimateMoofCount(dur, part, nominal)
			}
			seg.fmp4.Duration = dur
			if seg.fmp4.codecID == 0 && tracks != nil {
				seg.fmp4.codecID = internCodecID(pe, internTracks(pe, tracks))
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
	if dirty {
		// Journal stays open mid-day; only refresh meta for recording_status.
		idx.writeMeta(pathName)
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
	for name := range idx.paths {
		n += idx.pathSegmentCountLocked(name)
	}
	return n
}

func (idx *Index) pathSegmentCount(pathName string) int {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	return idx.pathSegmentCountLocked(pathName)
}

func (idx *Index) pathSegmentCountLocked(pathName string) int {
	pe := idx.paths[pathName]
	if pe == nil {
		return 0
	}
	if pe.complete && len(pe.days) > 0 {
		n := 0
		for _, d := range pe.days {
			n += int(d.NSeg)
		}
		return n
	}
	return len(pe.segments)
}

// SetFMP4Meta stores inspected fMP4 playlist metadata for a segment.
func (idx *Index) SetFMP4Meta(pathName, fpath string, meta fmp4SegMeta, tracks ...[]*fmp4.InitTrack) {
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
	var tr []*fmp4.InitTrack
	if len(tracks) > 0 {
		tr = tracks[0]
	}
	if interned := internTracks(pe, metaTracks(meta, tr)); len(interned) > 0 {
		meta.codecID = internCodecID(pe, interned)
	}
	common := pe.commonFor(fpath)
	seg.Rel = segmentRelFast(common, fpath)
	seg.common = common
	seg.codecs = &pe.internedTracks
	seg.fmp4 = meta
	pe.rangesOK = false
}

func metaTracks(meta fmp4SegMeta, tracks []*fmp4.InitTrack) []*fmp4.InitTrack {
	if len(tracks) > 0 {
		return tracks
	}
	return codecTracks(nil, meta.codecID)
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
	common := pe.commonFor(fpath)
	if existing, ok := pe.byName[name]; ok {
		existing.Rel = segmentRelFast(common, fpath)
		existing.common = common
		existing.codecs = &pe.internedTracks
		if !existing.Start.Equal(start) {
			existing.Start = start
			sort.Slice(pe.segments, func(i, j int) bool {
				return pe.segments[i].Start.Before(pe.segments[j].Start)
			})
			pe.rangesOK = false
		}
		return
	}

	seg := bindSeg(pe, fpath, start)
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
		if seg.Fpath() != want && seg.Name() != name {
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
	forgetFMP4FileCaches(seg.Fpath())
}

func (idx *Index) compactPath(pathName string) {
	idx.sealOpenDay(pathName)
	idx.writeMeta(pathName)
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
	idx.debug.noteUpsert()
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
	name := filepath.Base(fpath)
	seg, ok := pe.byName[name]
	if !ok || !seg.fmp4.Ready {
		return
	}
	day := dvrDayDate(seg.Start)
	if pe.openDay != day && pe.openDay != "" {
		idx.mutex.Unlock()
		idx.compactOpenDay(pathName)
		idx.mutex.Lock()
		pe = idx.paths[pathName]
		if pe == nil {
			return
		}
		seg, ok = pe.byName[name]
		if !ok || !seg.fmp4.Ready {
			return
		}
	}
	idx.bindPersistLocked(pathName, pe, day, fpath)
	if pe.persist == nil || !pe.persist.ready {
		return
	}
	idx.persistCodecsLocked(pe)
	rec := dvrSegRec{
		Rel:      seg.Rel,
		Start:    seg.Start,
		Duration: seg.fmp4.Duration,
		Moof:     seg.fmp4.MoofCount,
		CodecID:  seg.fmp4.codecID,
		Ready:    true,
	}
	_ = pe.persist.writeOp(dvrJournalOp{Op: dvrOpUpsert, Seg: rec})
	day = dvrDayDate(seg.Start)
	common := pe.commonFor(fpath)
	// First Ready persist only: bump incremental caches. writeMeta just flushes them.
	if !seg.countedInMeta {
		seg.countedInMeta = true
		pe.appendDiskRange(common, seg.Start, trustedSegDuration(seg.fmp4.Duration, 0, pe.segmentDuration))
		pe.setDayNSeg(day, pe.dayNSeg(day)+1)
		pe.setDiskDayNSeg(common, day, pe.diskDayNSeg(common, day)+1)
	}
	ops := pe.persist.journalOps
	if dvrMetaEvery > 0 && ops > 0 && (ops == 1 || ops%dvrMetaEvery == 0) {
		idx.mutex.Unlock()
		idx.writeMeta(pathName)
		idx.mutex.Lock()
	}
}

func (idx *Index) persistDeleteLocked(pe *pathIndex, fpath string, day string) {
	if pe == nil {
		return
	}
	pe.selectLayoutForFpath(fpath)
	common := pe.commonFor(fpath)
	if day == "" {
		day = pe.openDay
	}
	if day == "" {
		day = dvrDayDate(time.Now())
	}
	rel := dvrRelPath(common, fpath)
	op := dvrJournalOp{Op: dvrOpDelete, Seg: dvrSegRec{Rel: rel}}

	// Prefer the live open journal when deleting today's segment on the bound disk.
	if pe.persist != nil && pe.persist.ready && pe.openDay == day &&
		pe.persist.journalPath == pe.layout.dayJournal(day) {
		_ = pe.persist.writeOp(op)
		return
	}
	hash := uint64(0)
	if pe.persist != nil {
		hash = pe.persist.hash
	} else if idx.pathConfs != nil {
		// best-effort: hash from first path conf match is not available; skip durable delete
		return
	}
	_ = appendJournalOpFile(pe.layout.dayJournal(day), hash, op)
}

func (idx *Index) ensurePersistLocked(pathName string, pe *pathIndex) {
	day := pe.openDay
	if day == "" {
		day = dvrDayDate(time.Now())
	}
	idx.bindPersistLocked(pathName, pe, day, "")
}

// ClosePersistResult is returned by ClosePersist.
type ClosePersistResult struct {
	Paths int // paths that had an open persist handle
	Dirty int // paths whose journal had pending ops (fsynced)
}

// ClosePersist fsyncs and closes per-path journals. Day indexes are append-only
// journals; snapshots are not rewritten on shutdown. Next start replays journals.
func (idx *Index) ClosePersist() ClosePersistResult {
	idx.mutex.Lock()
	type flushJob struct {
		name  string
		dirty bool
	}
	jobs := make([]flushJob, 0, len(idx.paths))
	for name, pe := range idx.paths {
		dirty := pe != nil && pe.persist != nil && pe.persist.journalOps > 0
		jobs = append(jobs, flushJob{name: name, dirty: dirty})
	}
	idx.mutex.Unlock()
	if len(jobs) == 0 {
		return ClosePersistResult{}
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}

	var dirtyN atomic.Int32
	var wg sync.WaitGroup
	ch := make(chan flushJob, len(jobs))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				if j.dirty {
					dirtyN.Add(1)
				}
				idx.mutex.Lock()
				var f *os.File
				needSync := false
				if pe := idx.paths[j.name]; pe != nil && pe.persist != nil {
					f, needSync = pe.persist.takeJournal()
					pe.persist.ready = false
				}
				idx.mutex.Unlock()
				if f != nil {
					if needSync {
						_ = f.Sync()
					}
					_ = f.Close()
				}
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	return ClosePersistResult{Paths: len(jobs), Dirty: int(dirtyN.Load())}
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
	idx.debug.noteCreate()
	start, ok := idx.decodeStart(pathName, fpath)
	if !ok {
		return
	}
	idx.Add(pathName, fpath, start)
}

// CompleteSegment records a closed segment using duration from the recorder.
// The file is not parsed when codec tracks are already interned for the path.
func (idx *Index) CompleteSegment(pathName, fpath string, duration time.Duration) {
	t0 := time.Now()
	idx.debug.noteComplete()
	defer func() {
		idx.logSlow("CompleteSegment", pathName, time.Since(t0), filepath.Base(fpath))
	}()
	if pathName == "" || fpath == "" {
		return
	}
	start, ok := idx.decodeStart(pathName, fpath)
	if !ok {
		return
	}
	day := dvrDayDate(start)
	idx.mutex.RLock()
	prev := ""
	if pe := idx.paths[pathName]; pe != nil && pe.openDay != "" && pe.openDay != day {
		prev = pe.openDay
	}
	idx.mutex.RUnlock()
	if prev != "" {
		idx.compactOpenDay(pathName)
		idx.writeMeta(pathName)
	}
	idx.Add(pathName, fpath, start)
	idx.bindPersistFile(pathName, day, fpath)

	idx.mutex.RLock()
	pathConfs := idx.pathConfs
	idx.mutex.RUnlock()
	if pathConfs == nil {
		return
	}
	pathConf, _, err := conf.FindPathConf(pathConfs, pathName)
	if err != nil {
		return
	}

	nominal := time.Duration(pathConf.RecordSegmentDuration)
	part := time.Duration(pathConf.RecordPartDuration)
	if duration <= 0 {
		duration = nominal
	}
	if duration <= 0 {
		return
	}

	// Duration/Ready must be set for both formats: day snapshots skip !Ready
	// segments, so MPEG-TS archive playlists would otherwise go empty after
	// compact/reload even though .ts files exist on disk.
	meta := fmp4SegMeta{
		Duration: duration,
		Ready:    true,
	}
	var tracks []*fmp4.InitTrack
	if pathConf.RecordFormat == conf.RecordFormatFMP4 {
		meta.MoofCount = estimateMoofCount(duration, part, nominal)
		tracks = idx.internedTracksOf(pathName)
		if len(tracks) == 0 {
			tIns := time.Now()
			ins, tr, ierr := inspectFMP4Segment(fpath)
			idx.debug.noteInspect(time.Since(tIns))
			if ierr == nil && ins.Ready {
				ins.Duration = duration
				if ins.MoofCount == 0 {
					ins.MoofCount = meta.MoofCount
				}
				meta = ins
				tracks = tr
			}
		}
	}
	idx.SetFMP4Meta(pathName, fpath, meta, tracks)
	idx.PersistUpsert(pathName, fpath)
}

// Remove deletes a segment by absolute file path.
func (idx *Index) Remove(fpath string) {
	t0 := time.Now()
	idx.debug.noteRemove()
	pathNameOut := ""
	defer func() {
		idx.logSlow("Remove", pathNameOut, time.Since(t0), filepath.Base(fpath))
	}()
	if fpath == "" {
		return
	}
	name := filepath.Base(fpath)
	clean := filepath.Clean(fpath)

	idx.mutex.Lock()
	pathName := ""
	for namePath, pe := range idx.paths {
		seg, ok := pe.byName[name]
		if !ok {
			continue
		}
		if seg.Fpath() != fpath && filepath.Clean(seg.Fpath()) != clean {
			continue
		}
		day := dvrDayDate(seg.Start)
		common := pe.commonFor(fpath)
		idx.persistDeleteLocked(pe, fpath, day)
		delete(pe.byName, name)
		for i, s := range pe.segments {
			if s == seg {
				pe.segments = append(pe.segments[:i], pe.segments[i+1:]...)
				break
			}
		}
		if seg.countedInMeta {
			pe.setDayNSeg(day, pe.dayNSeg(day)-1)
			pe.setDiskDayNSeg(common, day, pe.diskDayNSeg(common, day)-1)
			seg.countedInMeta = false
		}
		pe.rangesOK = false
		forgetFMP4FileCaches(fpath)
		forgetFMP4FileCaches(clean)
		delete(idx.dayCache, dayCacheKey{path: namePath, day: day})
		pathName = namePath
		break
	}
	idx.mutex.Unlock()
	pathNameOut = pathName
	if pathName != "" {
		idx.mutex.RLock()
		pe := idx.paths[pathName]
		write := pe != nil && pe.complete && pe.persist != nil
		idx.mutex.RUnlock()
		if write {
			idx.writeMeta(pathName)
		}
	}
}

// Ranges returns cached recording ranges for a path.
func (idx *Index) Ranges(pathName string) []RecordingRange {
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	if pe == nil {
		idx.mutex.RUnlock()
		return []RecordingRange{}
	}
	// Hot path: complete index already has incremental pe.ranges.
	if pe.complete && pe.rangesOK {
		out := idx.rangesSnapshotLocked(pe)
		idx.mutex.RUnlock()
		return out
	}
	if !pe.complete && len(pe.segments) == 0 {
		idx.mutex.RUnlock()
		return []RecordingRange{}
	}
	needBuild := !pe.complete && !pe.rangesOK && len(pe.segments) > 0
	idx.mutex.RUnlock()
	if needBuild {
		idx.mutex.Lock()
		pe = idx.paths[pathName]
		if pe != nil && !pe.complete && !pe.rangesOK && len(pe.segments) > 0 {
			pe.ranges = buildRanges(pe.timedSegsLocked(time.Now()), pe.segmentDuration, time.Now())
			pe.rangesOK = true
		}
		if pe == nil {
			idx.mutex.Unlock()
			return []RecordingRange{}
		}
		out := idx.rangesSnapshotLocked(pe)
		idx.mutex.Unlock()
		return out
	}

	idx.mutex.RLock()
	pe = idx.paths[pathName]
	if pe == nil {
		idx.mutex.RUnlock()
		return []RecordingRange{}
	}
	out := idx.rangesSnapshotLocked(pe)
	idx.mutex.RUnlock()
	return out
}

func (idx *Index) rangesSnapshotLocked(pe *pathIndex) []RecordingRange {
	if pe == nil || len(pe.ranges) == 0 {
		return []RecordingRange{}
	}
	out := make([]RecordingRange, len(pe.ranges))
	copy(out, pe.ranges)

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
		var nextDelta time.Duration
		if i+1 < len(pe.segments) {
			nextDelta = pe.segments[i+1].Start.Sub(s.Start)
		}
		stored := time.Duration(0)
		if s.fmp4.Ready {
			stored = s.fmp4.Duration
		}
		dur := trustedSegDuration(stored, nextDelta, pe.segmentDuration)
		if i+1 >= len(pe.segments) && stored <= 0 {
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
	pe := idx.paths[pathName]
	if pe == nil {
		idx.mutex.RUnlock()
		return nil
	}
	days := daysForWindow(pe.days, start, end)
	liveOnly := len(days) == 0 && len(pe.segments) > 0
	nominal := pe.segmentDuration
	idx.mutex.RUnlock()
	if nominal <= 0 {
		nominal = time.Hour
	}

	collect := func(segs []*IndexedSegment) []*IndexedSegment {
		var out []*IndexedSegment
		for _, s := range segs {
			if s.Start.After(end) {
				continue
			}
			inWindow := !s.Start.Before(start) && !s.Start.After(end)
			if inWindow || segmentOverlaps(s, start, end, nominal) {
				cp := *s
				out = append(out, &cp)
			}
		}
		return out
	}

	if liveOnly {
		idx.mutex.RLock()
		defer idx.mutex.RUnlock()
		pe = idx.paths[pathName]
		if pe == nil {
			return nil
		}
		return collect(pe.segments)
	}

	var out []*IndexedSegment
	for _, day := range days {
		out = append(out, collect(idx.segsForDay(pathName, day))...)
	}
	// Days listed in meta but not yet pinned/cached (or only RAM live segs):
	// fall back to in-memory segments so playlist never goes empty while recording.
	if len(out) == 0 {
		idx.mutex.RLock()
		defer idx.mutex.RUnlock()
		pe = idx.paths[pathName]
		if pe == nil {
			return nil
		}
		return collect(pe.segments)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
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
func dayFromFileName(name string) (string, bool) {
	if len(name) >= 10 && name[4] == '-' && name[7] == '-' {
		day := name[:10]
		if isDayDirName(day) {
			return day, true
		}
	}
	return "", false
}

func (idx *Index) FindNearest(pathName string, t time.Time) (*IndexedSegment, bool) {
	day := dvrDayDate(t)
	segs := idx.segsForDay(pathName, day)
	if len(segs) == 0 {
		idx.mutex.RLock()
		pe := idx.paths[pathName]
		prev := ""
		if pe != nil {
			for _, d := range pe.days {
				if d.Date >= day {
					break
				}
				prev = d.Date
			}
			if len(segs) == 0 && len(pe.days) == 0 && len(pe.segments) > 0 {
				copied := make([]*IndexedSegment, len(pe.segments))
				for i, s := range pe.segments {
					cp := *s
					copied[i] = &cp
				}
				idx.mutex.RUnlock()
				segs = copied
			} else {
				idx.mutex.RUnlock()
			}
		} else {
			idx.mutex.RUnlock()
		}
		if prev != "" && len(segs) == 0 {
			segs = idx.segsForDay(pathName, prev)
		}
	}
	if len(segs) == 0 {
		return nil, false
	}
	i := sort.Search(len(segs), func(i int) bool {
		return segs[i].Start.After(t)
	})
	if i == 0 {
		seg := *segs[0]
		return &seg, true
	}
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
			if tr := seg.tracks(); len(tr) > 0 {
				idx.mutex.RUnlock()
				return tr
			}
			if p := seg.Fpath(); p != "" {
				fpath = p
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
			seg.fmp4.codecID = internCodecID(pe, tracks)
			seg.codecs = &pe.internedTracks
		}
	}
	idx.mutex.Unlock()
	return tracks
}

// FindLatest returns the most recent segment for a path.
func (idx *Index) FindLatest(pathName string) (*IndexedSegment, bool) {
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	if pe == nil {
		idx.mutex.RUnlock()
		return nil, false
	}
	if n := len(pe.segments); n > 0 {
		seg := *pe.segments[n-1]
		idx.mutex.RUnlock()
		return &seg, true
	}
	last := pe.lastDay()
	idx.mutex.RUnlock()
	if last == "" {
		return nil, false
	}
	segs := idx.segsForDay(pathName, last)
	if len(segs) == 0 {
		return nil, false
	}
	seg := *segs[len(segs)-1]
	return &seg, true
}

// FindByName returns the absolute path of a segment basename, if present.
func (idx *Index) FindByName(pathName, fileName string) (string, bool) {
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	if pe != nil {
		if seg, ok := pe.byName[fileName]; ok {
			fpath := seg.Fpath()
			idx.mutex.RUnlock()
			return fpath, true
		}
	}
	idx.mutex.RUnlock()
	day, ok := dayFromFileName(fileName)
	if !ok {
		return "", false
	}
	for _, s := range idx.segsForDay(pathName, day) {
		if s.Name() == fileName {
			return s.Fpath(), true
		}
	}
	return "", false
}

func (idx *Index) ensurePathLocked(pathName string) *pathIndex {
	pe := idx.paths[pathName]
	if pe != nil {
		idx.fillLayoutsLocked(pe, pathName)
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
		pinnedDays:      make(map[string]struct{}),
		segmentDuration: dur,
	}
	idx.paths[pathName] = pe
	idx.fillLayoutsLocked(pe, pathName)
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

	fpathAbs, _ := filepath.Abs(fpath)
	for _, raw := range pathConf.RecordPathFormats() {
		recordPath := recordstore.PathAddExtension(
			strings.ReplaceAll(raw, "%path", pathName),
			pathConf.RecordFormat,
		)
		recordPath, _ = filepath.Abs(recordPath)
		var pa recordstore.Path
		if pa.Decode(recordPath, fpathAbs) {
			return pa.Start, true
		}
	}
	return time.Time{}, false
}
