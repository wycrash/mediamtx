package compatapi

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediamtx/internal/conf"
)

type dayCacheKey struct {
	path string
	day  string
}

type loadedDay struct {
	segs   []*IndexedSegment
	byName map[string]*IndexedSegment
	codecs [][]*fmp4.InitTrack
}

func codecIDFrom(codecs [][]*fmp4.InitTrack, tracks []*fmp4.InitTrack) uint8 {
	if len(tracks) == 0 {
		return 0
	}
	for i, existing := range codecs {
		if len(existing) == 0 {
			continue
		}
		if sameTracksPtr(existing, tracks) || fmp4TracksCompatible(existing, tracks) {
			return uint8(i + 1)
		}
	}
	return 0
}

func segsFromSnapshot(common string, snap dvrSnapshot) []*IndexedSegment {
	out := make([]*IndexedSegment, 0, len(snap.Segs))
	codecs := snap.Codecs
	for _, rec := range snap.Segs {
		rel := rec.Rel
		if rel == "" {
			continue
		}
		seg := &IndexedSegment{
			Rel:    filepath.ToSlash(rel),
			Start:  rec.Start,
			common: common,
			codecs: &codecs,
			fmp4: fmp4SegMeta{
				Duration:  rec.Duration,
				MoofCount: rec.Moof,
				codecID:   rec.CodecID,
				Ready:     rec.Ready,
			},
		}
		if !seg.fmp4.Ready && (seg.fmp4.Duration > 0 || seg.fmp4.MoofCount > 0 || seg.fmp4.codecID > 0) {
			seg.fmp4.Ready = true
		}
		if seg.fmp4.Ready {
			seg.countedInMeta = true
		}
		out = append(out, seg)
	}
	return out
}

func snapshotFromSegs(hash uint64, common string, segs []*IndexedSegment, codecs [][]*fmp4.InitTrack) dvrSnapshot {
	s := dvrSnapshot{
		Hash:   hash,
		Codecs: append([][]*fmp4.InitTrack(nil), codecs...),
		Segs:   make([]dvrSegRec, 0, len(segs)),
	}
	for _, seg := range segs {
		if !seg.fmp4.Ready {
			continue
		}
		rel := seg.Rel
		if rel == "" {
			rel = seg.Name()
		}
		if common != "" && filepath.IsAbs(filepath.FromSlash(rel)) {
			rel = dvrRelPath(common, seg.Fpath())
		}
		s.Segs = append(s.Segs, dvrSegRec{
			Rel:      rel,
			Start:    seg.Start,
			Duration: seg.fmp4.Duration,
			Moof:     seg.fmp4.MoofCount,
			CodecID:  seg.fmp4.codecID,
			Ready:    true,
		})
	}
	return s
}

func internTracksInto(codecs *[][]*fmp4.InitTrack, tracks []*fmp4.InitTrack) []*fmp4.InitTrack {
	if len(tracks) == 0 {
		return tracks
	}
	for _, existing := range *codecs {
		if len(existing) == 0 {
			continue
		}
		if fmp4TracksCompatible(existing, tracks) {
			return existing
		}
	}
	*codecs = append(*codecs, tracks)
	return tracks
}

func fillSegsMeta(segs []*IndexedSegment, tracks []*fmp4.InitTrack, codecs [][]*fmp4.InitTrack, nominal, part time.Duration) {
	if nominal <= 0 {
		nominal = time.Hour
	}
	id := codecIDFrom(codecs, tracks)
	cap := segDurationCap(nominal)
	for i, seg := range segs {
		var nextDelta time.Duration
		if i+1 < len(segs) {
			nextDelta = segs[i+1].Start.Sub(seg.Start)
		}
		dur := trustedSegDuration(seg.fmp4.Duration, nextDelta, nominal)
		if !seg.fmp4.Ready || seg.fmp4.Duration == 0 || seg.fmp4.Duration > cap || (seg.fmp4.codecID == 0 && id != 0) {
			if seg.fmp4.MoofCount == 0 {
				seg.fmp4.MoofCount = estimateMoofCount(dur, part, nominal)
			}
			seg.fmp4.Duration = dur
			if seg.fmp4.codecID == 0 {
				seg.fmp4.codecID = id
			}
			seg.fmp4.Ready = true
		}
	}
}

func mergeRecordingRanges(dst, src []RecordingRange, nominal time.Duration) []RecordingRange {
	all := append(append([]RecordingRange(nil), dst...), src...)
	sort.Slice(all, func(i, j int) bool { return all[i].From < all[j].From })
	var out []RecordingRange
	for _, r := range all {
		out = appendRecordingRangeOrdered(out, r.From, r.Duration, nominal)
	}
	return out
}

func appendRecordingRange(ranges []RecordingRange, start time.Time, dur, nominal time.Duration) []RecordingRange {
	if start.IsZero() {
		return ranges
	}
	if dur <= 0 {
		dur = nominal
	}
	if dur <= 0 {
		dur = time.Second
	}
	from := start.Unix()
	dsec := int64((dur + time.Second/2) / time.Second)
	if dsec < 1 {
		dsec = 1
	}
	if len(ranges) > 0 && from < ranges[len(ranges)-1].From {
		return mergeRecordingRanges(ranges, []RecordingRange{{From: from, Duration: dsec}}, nominal)
	}
	return appendRecordingRangeOrdered(ranges, from, dsec, nominal)
}

func appendRecordingRangeOrdered(ranges []RecordingRange, from, dsec int64, nominal time.Duration) []RecordingRange {
	if dsec < 1 {
		dsec = 1
	}
	tol := int64((rangeMergeTolerance(nominal) + time.Second/2) / time.Second)
	if len(ranges) == 0 {
		return []RecordingRange{{From: from, Duration: dsec}}
	}
	last := &ranges[len(ranges)-1]
	gap := from - last.closedAt()
	if gap <= tol {
		end := from + dsec
		if end > last.closedAt() {
			last.Duration = end - last.From
		}
		return ranges
	}
	return append(ranges, RecordingRange{From: from, Duration: dsec})
}

func (pe *pathIndex) appendSegRange(start time.Time, dur time.Duration) {
	pe.ranges = appendRecordingRange(pe.ranges, start, dur, pe.segmentDuration)
	pe.rangesOK = true
}

func rangesCoverDays(ranges []RecordingRange, days []dvrDayInfo) bool {
	if len(days) == 0 {
		return true
	}
	if len(ranges) == 0 {
		return false
	}
	first, err := time.ParseInLocation("2006-01-02", days[0].Date, time.Local)
	if err != nil {
		return true
	}
	last, err := time.ParseInLocation("2006-01-02", days[len(days)-1].Date, time.Local)
	if err != nil {
		return true
	}
	if ranges[0].From >= first.Add(24*time.Hour).Unix() {
		return false
	}
	return ranges[len(ranges)-1].closedAt() >= last.Unix()
}

func (idx *Index) rangesNeedRepair(pathName string) bool {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	pe := idx.paths[pathName]
	if pe == nil || !pe.complete || len(pe.days) == 0 {
		return false
	}
	return len(pe.ranges) == 0
}

func (idx *Index) rebuildRangesFromDayFiles(pathName string, persist bool) {
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	if pe == nil || len(pe.days) == 0 {
		idx.mutex.RUnlock()
		return
	}
	days := append([]dvrDayInfo(nil), pe.days...)
	nominal := pe.segmentDuration
	idx.mutex.RUnlock()

	diskRanges := make(map[string][]RecordingRange)
	var ranges []RecordingRange
	for _, d := range days {
		segs := idx.loadDaySegs(pathName, d.Date)
		for i, seg := range segs {
			var nextDelta time.Duration
			if i+1 < len(segs) {
				nextDelta = segs[i+1].Start.Sub(seg.Start)
			}
			dur := trustedSegDuration(seg.fmp4.Duration, nextDelta, nominal)
			ranges = appendRecordingRange(ranges, seg.Start, dur, nominal)
			if c := seg.common; c != "" {
				diskRanges[c] = appendRecordingRange(diskRanges[c], seg.Start, dur, nominal)
			}
		}
	}
	if len(ranges) == 0 {
		return
	}
	idx.mutex.Lock()
	if pe := idx.paths[pathName]; pe != nil {
		pe.ranges = ranges
		pe.rangesOK = true
		pe.diskRanges = diskRanges
	}
	idx.mutex.Unlock()
	if persist {
		idx.writeMeta(pathName)
	}
}

func (pe *pathIndex) setDayNSeg(day string, n int) {
	if day == "" {
		return
	}
	if n < 0 {
		n = 0
	}
	for i := range pe.days {
		if pe.days[i].Date == day {
			if n == 0 {
				pe.days = append(pe.days[:i], pe.days[i+1:]...)
				return
			}
			pe.days[i].NSeg = uint32(n)
			return
		}
	}
	if n == 0 {
		return
	}
	pe.days = append(pe.days, dvrDayInfo{Date: day, NSeg: uint32(n)})
	sort.Slice(pe.days, func(i, j int) bool { return pe.days[i].Date < pe.days[j].Date })
}

func (pe *pathIndex) removeDay(day string) {
	for i, d := range pe.days {
		if d.Date == day {
			pe.days = append(pe.days[:i], pe.days[i+1:]...)
			return
		}
	}
}

func (pe *pathIndex) lastDay() string {
	if len(pe.days) == 0 {
		return ""
	}
	return pe.days[len(pe.days)-1].Date
}

func (pe *pathIndex) metaSnapshot() dvrMeta {
	m := dvrMeta{Ranges: append([]RecordingRange(nil), pe.ranges...), Days: append([]dvrDayInfo(nil), pe.days...)}
	if pe.persist != nil {
		m.Hash = pe.persist.hash
	}
	return m
}

func dayOverlapsWindow(day string, start, end time.Time) bool {
	t, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		return false
	}
	dayEnd := t.Add(24 * time.Hour)
	return t.Before(end) && dayEnd.After(start)
}

func daysForWindow(days []dvrDayInfo, start, end time.Time) []string {
	var out []string
	for _, d := range days {
		if dayOverlapsWindow(d.Date, start, end) {
			out = append(out, d.Date)
		}
	}
	return out
}

func (pe *pathIndex) dayIsPinned(day string) bool {
	if day == "" {
		return false
	}
	if pe.pinnedDays != nil {
		_, ok := pe.pinnedDays[day]
		return ok
	}
	return false
}

func (idx *Index) writeMeta(pathName string) {
	t0 := time.Now()
	nSeg := 0
	nLayouts := 0
	defer func() {
		d := time.Since(t0)
		idx.debug.noteWriteMeta(d)
		idx.logSlow("writeMeta", pathName, d,
			fmt.Sprintf("segments=%d layouts=%d", nSeg, nLayouts))
	}()

	idx.mutex.Lock()
	pe := idx.paths[pathName]
	if pe == nil {
		idx.mutex.Unlock()
		return
	}
	layouts := append([]dvrPathLayout(nil), pe.allLayouts()...)
	nLayouts = len(layouts)
	nSeg = len(pe.segments)
	if len(layouts) == 0 {
		idx.mutex.Unlock()
		return
	}
	hash := uint64(0)
	if pe.persist != nil {
		hash = pe.persist.hash
	}
	items := make([]struct {
		path string
		m    dvrMeta
	}, 0, len(layouts))
	for _, l := range layouts {
		if l.meta == "" {
			continue
		}
		// diskRanges / diskDays are maintained incrementally (appendDiskRange,
		// setDiskDayNSeg, rebuildRangesFromDayFiles). Re-merging every live
		// segment here was O(N²): each older Start triggered mergeRecordingRanges
		// against an already-complete diskRanges tail (see pprof CompleteSegment).
		m := dvrMeta{
			Hash:   hash,
			Ranges: append([]RecordingRange(nil), pe.diskRanges[l.common]...),
		}
		if days := pe.diskDays[l.common]; len(days) > 0 {
			m.Days = make([]dvrDayInfo, 0, len(days))
			for day, n := range days {
				m.Days = append(m.Days, dvrDayInfo{Date: day, NSeg: uint32(n)})
			}
			sort.Slice(m.Days, func(i, j int) bool { return m.Days[i].Date < m.Days[j].Date })
		} else if len(pe.days) > 0 && (l.common == pe.commonPath || pe.commonPath == "") {
			// Single-disk / unset common: fall back to path-level day counters.
			m.Days = append([]dvrDayInfo(nil), pe.days...)
		}
		items = append(items, struct {
			path string
			m    dvrMeta
		}{path: l.meta, m: m})
	}
	idx.mutex.Unlock()
	for _, it := range items {
		_ = writeMetaFile(it.path, it.m)
	}
}

// sealOpenDay closes the open-day journal (fsync) without rewriting a snapshot.
// The journal file is the durable day index; next startup replays it.
func (idx *Index) sealOpenDay(pathName string) {
	idx.mutex.Lock()
	pe := idx.paths[pathName]
	if pe == nil || pe.persist == nil || pe.openDay == "" {
		idx.mutex.Unlock()
		return
	}
	day := pe.openDay
	n := 0
	for _, seg := range pe.segments {
		if dvrDayDate(seg.Start) == day && seg.fmp4.Ready {
			n++
		}
	}
	if n > 0 {
		pe.setDayNSeg(day, n)
	}
	p := pe.persist
	idx.mutex.Unlock()

	p.closeJournal()

	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe = idx.paths[pathName]
	if pe == nil || pe.persist != p {
		return
	}
	p.ready = false
}

// compactOpenDay is kept as a name used at day-change call sites; it seals the
// journal only (no MTXI snapshot dump).
func (idx *Index) compactOpenDay(pathName string) {
	idx.sealOpenDay(pathName)
}

func applyJournalOps(snap dvrSnapshot, ops []dvrJournalOp) dvrSnapshot {
	if len(ops) == 0 {
		return snap
	}
	byRel := make(map[string]int, len(snap.Segs)+len(ops))
	for i, rec := range snap.Segs {
		if rec.Rel != "" {
			byRel[rec.Rel] = i
		}
	}
	for _, op := range ops {
		switch op.Op {
		case dvrOpCodec:
			id := int(op.CodecID)
			if id <= 0 {
				continue
			}
			for len(snap.Codecs) < id {
				snap.Codecs = append(snap.Codecs, nil)
			}
			snap.Codecs[id-1] = op.Tracks
		case dvrOpUpsert:
			if op.Seg.Rel == "" {
				continue
			}
			if i, ok := byRel[op.Seg.Rel]; ok {
				snap.Segs[i] = op.Seg
				continue
			}
			byRel[op.Seg.Rel] = len(snap.Segs)
			snap.Segs = append(snap.Segs, op.Seg)
		case dvrOpDelete:
			i, ok := byRel[op.Seg.Rel]
			if !ok {
				continue
			}
			snap.Segs = append(snap.Segs[:i], snap.Segs[i+1:]...)
			delete(byRel, op.Seg.Rel)
			for j := i; j < len(snap.Segs); j++ {
				if snap.Segs[j].Rel != "" {
					byRel[snap.Segs[j].Rel] = j
				}
			}
		}
	}
	return snap
}

func cloneDaySnapshot(s dvrSnapshot) dvrSnapshot {
	out := dvrSnapshot{Hash: s.Hash}
	if len(s.Codecs) > 0 {
		out.Codecs = append([][]*fmp4.InitTrack(nil), s.Codecs...)
	}
	if len(s.Segs) > 0 {
		out.Segs = append([]dvrSegRec(nil), s.Segs...)
	}
	return out
}

func loadOneDaySnapshot(l dvrPathLayout, day string, hash uint64) (dvrSnapshot, []dvrJournalOp, bool) {
	snap, ops, ok, _ := loadOneDayIndex(l, day, hash)
	return snap, ops, ok
}

func loadOneDayIndex(l dvrPathLayout, day string, hash uint64) (dvrSnapshot, []dvrJournalOp, bool, bool) {
	snap, ops, ok, corrupt, _ := loadOneDayIndexDetail(l, day, hash)
	return snap, ops, ok, corrupt
}

func loadOneDayIndexDetail(l dvrPathLayout, day string, hash uint64) (dvrSnapshot, []dvrJournalOp, bool, bool, loadDayOp) {
	info := loadDayOp{day: day}
	if l.common == "" || day == "" {
		return dvrSnapshot{}, nil, false, false, info
	}

	tRead := time.Now()
	snapData, snapErr := os.ReadFile(l.daySnap(day))
	if snapErr == nil {
		info.snapB = int64(len(snapData))
	}
	jourData, jErr := os.ReadFile(l.dayJournal(day))
	if os.IsNotExist(jErr) {
		jourData, jErr = nil, nil
	}
	if jErr == nil {
		info.journalB = int64(len(jourData))
	}
	info.read = time.Since(tRead)

	tParse := time.Now()
	var snap dvrSnapshot
	if snapErr == nil {
		snap, snapErr = decodeSnapshot(snapData)
	}
	if snapErr != nil || (hash != 0 && snap.Hash != hash) {
		snap = dvrSnapshot{Hash: hash}
	}
	if jErr != nil {
		info.parse = time.Since(tParse)
		info.d = info.read + info.parse
		info.corrupt = true
		return dvrSnapshot{Hash: hash}, nil, false, true, info
	}
	ops, jErr := decodeJournalBytes(jourData, hash)
	if jErr != nil {
		info.parse = time.Since(tParse)
		info.d = info.read + info.parse
		info.corrupt = true
		return dvrSnapshot{Hash: hash}, nil, false, true, info
	}
	if len(snap.Segs) == 0 && len(ops) == 0 {
		info.parse = time.Since(tParse)
		info.d = info.read + info.parse
		return dvrSnapshot{}, ops, false, false, info
	}
	if len(ops) > 0 {
		snap = applyJournalOps(snap, ops)
	}
	if snap.Hash == 0 {
		snap.Hash = hash
	}
	info.parse = time.Since(tParse)
	info.d = info.read + info.parse
	info.segs = len(snap.Segs)
	info.ops = len(ops)
	info.ok = true
	return snap, ops, true, false, info
}

func (idx *Index) loadDaySegs(pathName, day string) []*IndexedSegment {
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	if pe == nil || day == "" {
		idx.mutex.RUnlock()
		return nil
	}
	layouts := append([]dvrPathLayout(nil), pe.allLayouts()...)
	hash := uint64(0)
	if pe.persist != nil {
		hash = pe.persist.hash
	}
	idx.mutex.RUnlock()

	var segs []*IndexedSegment
	for _, l := range layouts {
		snap, ok := idx.loadCachedDaySnapshot(l, day, hash)
		if !ok {
			continue
		}
		segs = append(segs, segsFromSnapshot(l.common, snap)...)
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].Start.Before(segs[j].Start) })
	return segs
}

func (idx *Index) loadDaySnapshot(pathName, day string) (dvrSnapshot, bool) {
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	if pe == nil || day == "" {
		idx.mutex.RUnlock()
		return dvrSnapshot{}, false
	}
	layouts := append([]dvrPathLayout(nil), pe.allLayouts()...)
	hash := uint64(0)
	if pe.persist != nil {
		hash = pe.persist.hash
	}
	idx.mutex.RUnlock()

	var merged dvrSnapshot
	ok := false
	for _, l := range layouts {
		snap, loaded := idx.loadCachedDaySnapshot(l, day, hash)
		if !loaded {
			continue
		}
		ok = true
		if merged.Hash == 0 {
			merged.Hash = snap.Hash
		}
		merged.Codecs = append(merged.Codecs, snap.Codecs...)
		merged.Segs = append(merged.Segs, snap.Segs...)
	}
	return merged, ok
}

func (idx *Index) beginSnapMemo() {
	if idx == nil {
		return
	}
	idx.snapMemoMu.Lock()
	idx.snapMemo = make(map[string]snapMemoEntry)
	idx.snapMemoMu.Unlock()
}

func (idx *Index) endSnapMemo() {
	if idx == nil {
		return
	}
	idx.snapMemoMu.Lock()
	idx.snapMemo = nil
	idx.snapMemoMu.Unlock()
}

func snapMemoKey(l dvrPathLayout, day string) string {
	return l.dayJournal(day)
}

func (idx *Index) loadCachedDaySnapshot(l dvrPathLayout, day string, hash uint64) (dvrSnapshot, bool) {
	key := snapMemoKey(l, day)
	if idx != nil && key != "" {
		idx.snapMemoMu.Lock()
		if idx.snapMemo != nil {
			if e, ok := idx.snapMemo[key]; ok {
				idx.snapMemoMu.Unlock()
				return cloneDaySnapshot(e.snap), e.ok
			}
		}
		idx.snapMemoMu.Unlock()
	}
	snap, ops, ok, corrupt := loadOneDayIndex(l, day, hash)
	if idx != nil && key != "" {
		idx.snapMemoMu.Lock()
		if idx.snapMemo != nil {
			idx.snapMemo[key] = snapMemoEntry{
				snap: cloneDaySnapshot(snap), ops: ops, ok: ok, corrupt: corrupt,
			}
		}
		idx.snapMemoMu.Unlock()
	}
	return snap, ok
}

func (idx *Index) cachedJournalOps(l dvrPathLayout, day string) ([]dvrJournalOp, bool) {
	if idx == nil {
		return nil, false
	}
	key := snapMemoKey(l, day)
	idx.snapMemoMu.Lock()
	defer idx.snapMemoMu.Unlock()
	if idx.snapMemo == nil {
		return nil, false
	}
	e, ok := idx.snapMemo[key]
	if !ok {
		return nil, false
	}
	return e.ops, true
}

// loadDiskMeta reads meta (list of day indexes) then each named day journal.
// A missing day file is skipped. A present but unreadable journal is queued
// for rebuild. Walk of recordings is not used here.
func (idx *Index) loadDiskMeta(l dvrPathLayout, hash uint64, tr *loadPathOps) (dvrMeta, []repairDay, []dvrDayInfo, bool) {
	if l.meta == "" {
		return dvrMeta{}, nil, nil, false
	}
	tMeta := time.Now()
	meta, err := readMetaFile(l.meta)
	if tr != nil {
		tr.meta += time.Since(tMeta)
	}
	if err != nil || meta.Hash != hash {
		return dvrMeta{}, nil, nil, false
	}
	if len(meta.Days) == 0 {
		return meta, nil, nil, true
	}
	var repairs []repairDay
	loaded := make([]dvrDayInfo, 0, len(meta.Days))
	tDays := time.Now()
	for _, d := range meta.Days {
		if d.Date == "" {
			continue
		}
		snap, _, ok, corrupt := idx.memoLoadDay(l, d.Date, hash, tr)
		if ok {
			d.NSeg = uint32(len(snap.Segs))
			loaded = append(loaded, d)
			continue
		}
		if corrupt {
			repairs = append(repairs, repairDay{common: l.common, day: d.Date})
		}
	}
	if tr != nil {
		tr.days += time.Since(tDays)
	}
	if len(loaded) == 0 && len(repairs) == 0 {
		return dvrMeta{}, nil, nil, false
	}
	if len(loaded) == 0 {
		return dvrMeta{}, nil, nil, false
	}
	return meta, repairs, loaded, true
}

func (idx *Index) memoLoadDay(l dvrPathLayout, day string, hash uint64, tr *loadPathOps) (dvrSnapshot, []dvrJournalOp, bool, bool) {
	key := snapMemoKey(l, day)
	if idx != nil && key != "" {
		idx.snapMemoMu.Lock()
		if idx.snapMemo != nil {
			if e, hit := idx.snapMemo[key]; hit {
				idx.snapMemoMu.Unlock()
				return cloneDaySnapshot(e.snap), e.ops, e.ok, e.corrupt
			}
		}
		idx.snapMemoMu.Unlock()
	}
	snap, ops, ok, corrupt, info := loadOneDayIndexDetail(l, day, hash)
	if tr != nil {
		tr.dayOps = append(tr.dayOps, info)
	}
	if idx != nil && key != "" {
		idx.snapMemoMu.Lock()
		if idx.snapMemo != nil {
			idx.snapMemo[key] = snapMemoEntry{
				snap: cloneDaySnapshot(snap), ops: ops, ok: ok, corrupt: corrupt,
			}
		}
		idx.snapMemoMu.Unlock()
	}
	return snap, ops, ok, corrupt
}

func (idx *Index) diskIndexHealthy(l dvrPathLayout, hash uint64) (dvrMeta, map[string]struct{}, bool) {
	meta, _, loaded, ok := idx.loadDiskMeta(l, hash, nil)
	if !ok {
		return dvrMeta{}, nil, false
	}
	present := make(map[string]struct{}, len(loaded))
	for _, d := range loaded {
		present[d.Date] = struct{}{}
	}
	return meta, present, true
}

func (idx *Index) loadDayToCache(pathName, day string) *loadedDay {
	key := dayCacheKey{path: pathName, day: day}
	idx.mutex.Lock()
	if pe := idx.paths[pathName]; pe != nil && pe.dayIsPinned(day) {
		ld := idx.pinnedAsLoadedLocked(pe, day)
		idx.mutex.Unlock()
		return ld
	}
	if ld, ok := idx.dayCache[key]; ok {
		idx.touchDayLRULocked(key)
		idx.mutex.Unlock()
		return ld
	}
	idx.mutex.Unlock()

	segs := idx.loadDaySegs(pathName, day)
	if len(segs) == 0 {
		return nil
	}
	idx.mutex.Lock()
	pe := idx.paths[pathName]
	if pe == nil {
		idx.mutex.Unlock()
		return nil
	}
	if pe.dayIsPinned(day) {
		ld := idx.pinnedAsLoadedLocked(pe, day)
		idx.mutex.Unlock()
		return ld
	}
	if ld, exists := idx.dayCache[key]; exists {
		idx.touchDayLRULocked(key)
		idx.mutex.Unlock()
		return ld
	}
	ld := &loadedDay{
		segs:   segs,
		byName: make(map[string]*IndexedSegment, len(segs)),
	}
	for _, s := range segs {
		ld.byName[s.Name()] = s
	}
	if idx.dayCache == nil {
		idx.dayCache = make(map[dayCacheKey]*loadedDay)
	}
	idx.dayCache[key] = ld
	idx.dayLRU = append(idx.dayLRU, key)
	for len(idx.dayCache) > dayCacheLimit {
		old := idx.dayLRU[0]
		idx.dayLRU = idx.dayLRU[1:]
		delete(idx.dayCache, old)
	}
	idx.mutex.Unlock()
	return ld
}

func (idx *Index) pinnedAsLoadedLocked(pe *pathIndex, day string) *loadedDay {
	ld := &loadedDay{byName: make(map[string]*IndexedSegment), codecs: pe.internedTracks}
	for _, s := range pe.segments {
		if dvrDayDate(s.Start) == day {
			ld.segs = append(ld.segs, s)
			ld.byName[s.Name()] = s
		}
	}
	return ld
}

func (idx *Index) touchDayLRULocked(key dayCacheKey) {
	for i, k := range idx.dayLRU {
		if k == key {
			idx.dayLRU = append(idx.dayLRU[:i], idx.dayLRU[i+1:]...)
			break
		}
	}
	idx.dayLRU = append(idx.dayLRU, key)
}

func (idx *Index) pinDay(pathName, day string) {
	if day == "" {
		return
	}
	segs := idx.loadDaySegs(pathName, day)
	if len(segs) == 0 {
		return
	}
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe := idx.paths[pathName]
	if pe == nil {
		return
	}
	if pe.pinnedDays == nil {
		pe.pinnedDays = make(map[string]struct{})
	}
	// Always merge disk segs even when already pinned. Live CompleteSegment
	// during rebuild calls bindPersist and marks the day pinned with only the
	// live edge; skipping here left archive windows empty after rebuild.
	for _, seg := range segs {
		if tr := seg.tracks(); len(tr) > 0 && len(pe.internedTracks) == 0 {
			pe.internedTracks = append(pe.internedTracks, tr)
		}
		idx.addLocked(pe, seg.Fpath(), seg.Start)
		if existing, ok := pe.byName[seg.Name()]; ok {
			existing.fmp4.Duration = seg.fmp4.Duration
			existing.fmp4.MoofCount = seg.fmp4.MoofCount
			existing.fmp4.Ready = seg.fmp4.Ready
			if tr := seg.tracks(); len(tr) > 0 {
				interned := internTracks(pe, tr)
				existing.fmp4.codecID = internCodecID(pe, interned)
			} else {
				existing.fmp4.codecID = seg.fmp4.codecID
			}
			existing.codecs = &pe.internedTracks
		}
	}
	pe.pinnedDays[day] = struct{}{}
	delete(idx.dayCache, dayCacheKey{path: pathName, day: day})
}

func (idx *Index) unpinDayLocked(pe *pathIndex, day string) {
	if pe == nil || day == "" || pe.openDay == day {
		return
	}
	if pe.lastDay() == day {
		return
	}
	kept := pe.segments[:0]
	for _, s := range pe.segments {
		if dvrDayDate(s.Start) == day {
			delete(pe.byName, s.Name())
			continue
		}
		kept = append(kept, s)
	}
	pe.segments = kept
	delete(pe.pinnedDays, day)
}

func (idx *Index) evictStalePinned(pathName string) {
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe := idx.paths[pathName]
	if pe == nil {
		return
	}
	cutoffDay := dvrDayDate(time.Now().Add(-reconcileEdgeWindow))
	today := dvrDayDate(time.Now())
	last := pe.lastDay()
	for day := range pe.pinnedDays {
		if day == pe.openDay || day == today || day == last || day >= cutoffDay {
			continue
		}
		idx.unpinDayLocked(pe, day)
	}
}

func (idx *Index) segsForDay(pathName, day string) []*IndexedSegment {
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	pinned := pe != nil && pe.dayIsPinned(day)
	idx.mutex.RUnlock()
	if pinned {
		idx.mutex.RLock()
		pe = idx.paths[pathName]
		var out []*IndexedSegment
		if pe != nil {
			for _, s := range pe.segments {
				if dvrDayDate(s.Start) == day {
					cp := *s
					out = append(out, &cp)
				}
			}
		}
		idx.mutex.RUnlock()
		if len(out) > 0 {
			return out
		}
		// Pinned but empty: fall through to disk (bindPersist without pinDay).
	}
	ld := idx.loadDayToCache(pathName, day)
	if ld == nil {
		return nil
	}
	out := make([]*IndexedSegment, len(ld.segs))
	for i, s := range ld.segs {
		cp := *s
		out[i] = &cp
	}
	return out
}

func (idx *Index) bindPersist(pathName, day string) {
	idx.bindPersistFile(pathName, day, "")
}

func (idx *Index) bindPersistFile(pathName, day, fpath string) {
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe := idx.paths[pathName]
	if pe == nil || day == "" {
		return
	}
	idx.bindPersistLocked(pathName, pe, day, fpath)
}

func (idx *Index) bindPersistLocked(pathName string, pe *pathIndex, day, fpath string) {
	if pe == nil || day == "" {
		return
	}
	if idx.pathConfs == nil {
		return
	}
	pathConf, _, err := conf.FindPathConf(idx.pathConfs, pathName)
	if err != nil {
		return
	}
	if pe.persist == nil {
		pe.persist = newDvrPersist(pathConf, pathName)
	}
	idx.fillLayoutsLocked(pe, pathName)
	if fpath != "" {
		pe.selectLayoutForFpath(fpath)
	}
	if pe.layout.common == "" {
		pe.setLayouts(makeDvrLayouts(pathConf, pathName))
	}
	if pe.openDay != "" && pe.openDay != day && day > pe.openDay {
		pe.internedTracks = nil
	}
	pe.openDay = day
	if pe.pinnedDays == nil {
		pe.pinnedDays = make(map[string]struct{})
	}
	pe.pinnedDays[day] = struct{}{}
	pe.persist.bindDay(pe.layout, day)
	_ = pe.persist.openJournalAppend()
}

func trimRangesBefore(ranges []RecordingRange, cutoff time.Time) []RecordingRange {
	if cutoff.IsZero() || len(ranges) == 0 {
		return ranges
	}
	cut := cutoff.Unix()
	out := ranges[:0]
	for _, r := range ranges {
		end := r.closedAt()
		if end <= cut {
			continue
		}
		if r.From < cut {
			r.Duration = end - cut
			r.From = cut
		}
		if r.Duration >= 1 {
			out = append(out, r)
		}
	}
	if out == nil {
		return []RecordingRange{}
	}
	return out
}

func (idx *Index) unlinkDayFiles(pe *pathIndex, day string) {
	if pe == nil || day == "" {
		return
	}
	for _, layout := range pe.allLayouts() {
		_ = os.Remove(layout.daySnap(day))
		_ = os.Remove(layout.dayJournal(day))
		if layout.dateDir {
			_ = os.Remove(filepath.Join(layout.common, day))
		}
	}
}
