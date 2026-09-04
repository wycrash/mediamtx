package compatapi

import (
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

func (idx *Index) rebuildRangesFromDayFiles(pathName string) {
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
	idx.writeMeta(pathName)
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
			pe.days[i].NSeg = uint32(n)
			return
		}
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
	idx.mutex.Lock()
	pe := idx.paths[pathName]
	if pe == nil {
		idx.mutex.Unlock()
		return
	}
	layouts := append([]dvrPathLayout(nil), pe.allLayouts()...)
	if len(layouts) == 0 {
		idx.mutex.Unlock()
		return
	}
	hash := uint64(0)
	if pe.persist != nil {
		hash = pe.persist.hash
	}
	type acc struct {
		days map[string]int
	}
	byDisk := make(map[string]*acc, len(layouts))
	for _, l := range layouts {
		byDisk[l.common] = &acc{days: make(map[string]int)}
	}
	liveDay := make(map[string]map[string]struct{}, len(layouts))
	for _, s := range pe.segments {
		c := s.common
		if c == "" {
			c = pe.commonPath
		}
		a := byDisk[c]
		if a == nil {
			continue
		}
		day := dvrDayDate(s.Start)
		a.days[day]++
		if liveDay[c] == nil {
			liveDay[c] = make(map[string]struct{})
		}
		liveDay[c][day] = struct{}{}
	}
	for common, stored := range pe.diskDays {
		a := byDisk[common]
		if a == nil {
			continue
		}
		for day, n := range stored {
			if _, live := liveDay[common][day]; live {
				continue
			}
			a.days[day] = n
		}
	}
	items := make([]struct {
		path string
		m    dvrMeta
	}, 0, len(layouts))
	for _, l := range layouts {
		if l.meta == "" {
			continue
		}
		a := byDisk[l.common]
		m := dvrMeta{Hash: hash}
		if a != nil {
			m.Ranges = append([]RecordingRange(nil), pe.diskRanges[l.common]...)
			for _, s := range pe.segments {
				c := s.common
				if c == "" {
					c = pe.commonPath
				}
				if c != l.common {
					continue
				}
				m.Ranges = appendRecordingRange(m.Ranges, s.Start, trustedSegDuration(s.fmp4.Duration, 0, pe.segmentDuration), pe.segmentDuration)
			}
			for day, n := range a.days {
				m.Days = append(m.Days, dvrDayInfo{Date: day, NSeg: uint32(n)})
			}
			sort.Slice(m.Days, func(i, j int) bool { return m.Days[i].Date < m.Days[j].Date })
			pe.loadDiskDays(l.common, m.Days)
			pe.storeDiskRanges(l.common, m.Ranges)
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

type compactJob struct {
	snapPath     string
	journalPath  string
	snap         dvrSnapshot
	boundPersist bool
}

func (idx *Index) compactOpenDay(pathName string) {
	idx.mutex.Lock()
	pe := idx.paths[pathName]
	if pe == nil || pe.persist == nil || pe.openDay == "" {
		idx.mutex.Unlock()
		return
	}
	day := pe.openDay
	layouts := append([]dvrPathLayout(nil), pe.allLayouts()...)
	if len(layouts) == 0 {
		idx.mutex.Unlock()
		return
	}
	hash := pe.persist.hash
	boundSnap := pe.persist.snapPath
	var jobs []compactJob
	total := 0
	for _, l := range layouts {
		var segs []*IndexedSegment
		for _, seg := range pe.segments {
			if dvrDayDate(seg.Start) != day {
				continue
			}
			if l.common != "" && seg.common != "" && seg.common != l.common {
				continue
			}
			segs = append(segs, seg)
		}
		if len(segs) == 0 {
			continue
		}
		snap := snapshotFromSegs(hash, l.common, segs, pe.internedTracks)
		total += len(snap.Segs)
		jobs = append(jobs, compactJob{
			snapPath:     l.daySnap(day),
			journalPath:  l.dayJournal(day),
			snap:         snap,
			boundPersist: boundSnap != "" && l.daySnap(day) == boundSnap,
		})
	}
	if len(jobs) == 0 {
		idx.mutex.Unlock()
		return
	}
	p := pe.persist
	pe.setDayNSeg(day, total)
	idx.mutex.Unlock()

	var boundFailed bool
	for _, j := range jobs {
		err := writeSnapshotFile(j.snapPath, j.snap)
		if err != nil {
			if j.boundPersist {
				boundFailed = true
			}
			continue
		}
		if !j.boundPersist {
			_ = writeEmptyJournal(j.journalPath, hash)
		}
	}

	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	pe = idx.paths[pathName]
	if pe == nil || pe.persist != p {
		return
	}
	if boundFailed {
		_ = p.openJournalAppend()
		return
	}
	_ = p.truncateJournal()
	for i := range pe.internedTracks {
		p.savedCodec[uint8(i+1)] = struct{}{}
	}
}

func writeEmptyJournal(path string, hash uint64) error {
	return writeFileAtomic(path, journalHeader(hash))
}

func applyJournalOps(snap dvrSnapshot, ops []dvrJournalOp) dvrSnapshot {
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
			replaced := false
			for i := range snap.Segs {
				if snap.Segs[i].Rel == op.Seg.Rel {
					snap.Segs[i] = op.Seg
					replaced = true
					break
				}
			}
			if !replaced {
				snap.Segs = append(snap.Segs, op.Seg)
			}
		case dvrOpDelete:
			filtered := snap.Segs[:0]
			for _, rec := range snap.Segs {
				if rec.Rel != op.Seg.Rel {
					filtered = append(filtered, rec)
				}
			}
			snap.Segs = filtered
		}
	}
	return snap
}

func loadOneDaySnapshot(l dvrPathLayout, day string, hash uint64) (dvrSnapshot, bool) {
	if l.common == "" || day == "" {
		return dvrSnapshot{}, false
	}
	snapPath := l.daySnap(day)
	journalPath := l.dayJournal(day)
	snap, err := readSnapshotFile(snapPath)
	if err != nil || (hash != 0 && snap.Hash != hash) {
		snap = dvrSnapshot{}
		if hash != 0 {
			ops, _ := readJournalFile(journalPath, hash)
			if len(ops) == 0 {
				return dvrSnapshot{}, false
			}
		} else {
			return dvrSnapshot{}, false
		}
	}
	if hash != 0 {
		ops, _ := readJournalFile(journalPath, hash)
		snap = applyJournalOps(snap, ops)
	}
	return snap, true
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
		snap, ok := loadOneDaySnapshot(l, day, hash)
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
		snap, loaded := loadOneDaySnapshot(l, day, hash)
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

// diskIndexHealthy is true when this disk's meta matches hash and, if it lists
// days, at least one day snapshot or journal can be loaded. Meta alone is not
// enough: a copied/stale meta with deleted .mtx-dvr-index* must rebuild.
func diskIndexHealthy(l dvrPathLayout, hash uint64) (dvrMeta, bool) {
	if l.meta == "" {
		return dvrMeta{}, false
	}
	meta, err := readMetaFile(l.meta)
	if err != nil || meta.Hash != hash {
		return dvrMeta{}, false
	}
	if len(meta.Days) == 0 {
		return meta, true
	}
	for _, d := range meta.Days {
		if d.Date == "" {
			continue
		}
		if _, ok := loadOneDaySnapshot(l, d.Date, hash); ok {
			return meta, true
		}
	}
	return dvrMeta{}, false
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
	if _, ok := pe.pinnedDays[day]; ok {
		return
	}
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
		defer idx.mutex.RUnlock()
		pe = idx.paths[pathName]
		if pe == nil {
			return nil
		}
		var out []*IndexedSegment
		for _, s := range pe.segments {
			if dvrDayDate(s.Start) == day {
				cp := *s
				out = append(out, &cp)
			}
		}
		return out
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
