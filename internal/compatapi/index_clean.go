package compatapi

import (
	"container/heap"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordcleaner"
)

// readDir is os.ReadDir; tests swap it to count listings.
var readDir = os.ReadDir

// Ensure Index satisfies the cleaner segment source interface.
var _ recordcleaner.SegmentLister = (*Index)(nil)

type cachedDirEnt struct {
	name  string
	isDir bool
}

// PathNames implements recordcleaner.SegmentLister.
// Returns complete indexed paths; ok=false when the index is empty/unusable.
func (idx *Index) PathNames() ([]string, bool) {
	if idx == nil {
		return nil, false
	}
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	if len(idx.paths) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(idx.paths))
	for name, pe := range idx.paths {
		if pe != nil && pe.complete {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	sort.Strings(out)
	return out, true
}

// SegmentsBefore implements recordcleaner.SegmentLister.
// Cold days are read from on-disk journals; pinned RAM is not the source of truth.
func (idx *Index) SegmentsBefore(pathName string, end time.Time) ([]recordcleaner.SegmentRef, bool) {
	if idx == nil || pathName == "" || end.IsZero() {
		return nil, false
	}
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	if pe == nil {
		idx.mutex.RUnlock()
		return nil, false
	}
	complete := pe.complete
	days := append([]dvrDayInfo(nil), pe.days...)
	idx.mutex.RUnlock()

	if !complete && len(days) == 0 {
		return nil, false
	}
	if len(days) == 0 {
		return idx.segmentsBeforeRAM(pathName, end), true
	}

	// One overlapping oldest day per tick: if days[0] is entirely after cutoff,
	// skip IO. Remaining older days are picked up on later ticks.
	for _, d := range days {
		if d.Date == "" || d.NSeg <= 0 {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02", d.Date, time.Local)
		if err != nil {
			continue
		}
		if t.After(end) {
			return nil, true
		}
		out := make([]recordcleaner.SegmentRef, 0)
		for _, seg := range idx.daySegsForClean(pathName, d.Date) {
			if seg == nil || seg.Start.After(end) {
				continue
			}
			out = append(out, recordcleaner.SegmentRef{
				PathName: pathName,
				Fpath:    seg.Fpath(),
				Start:    seg.Start,
			})
		}
		return out, true
	}
	return nil, true
}

func (idx *Index) segmentsBeforeRAM(pathName string, end time.Time) []recordcleaner.SegmentRef {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	pe := idx.paths[pathName]
	if pe == nil {
		return nil
	}
	n := sort.Search(len(pe.segments), func(i int) bool {
		return pe.segments[i].Start.After(end)
	})
	if n == 0 {
		return nil
	}
	out := make([]recordcleaner.SegmentRef, 0, n)
	for _, seg := range pe.segments[:n] {
		if seg == nil {
			continue
		}
		out = append(out, recordcleaner.SegmentRef{
			PathName: pathName,
			Fpath:    seg.Fpath(),
			Start:    seg.Start,
		})
	}
	return out
}

type cleanPathSnap struct {
	name     string
	days     []string
	layouts  []dvrPathLayout
	complete bool
}

// ReclaimCandidates implements recordcleaner.SegmentLister.
// Always pops the globally oldest remaining segment (oldest camera first).
// `limit` counts only files under diskRoot; older files on other disks of the
// pool are returned as companions so round-robin does not punch a hole.
func (idx *Index) ReclaimCandidates(storageName, diskRoot string, limit int) ([]recordcleaner.SegmentRef, bool) {
	if idx == nil || storageName == "" || diskRoot == "" || limit <= 0 {
		return nil, false
	}
	absDisk, err := filepath.Abs(diskRoot)
	if err != nil {
		absDisk = filepath.Clean(diskRoot)
	}

	snaps := idx.cleanSnaps(storageName)
	if len(snaps) == 0 {
		return nil, false
	}

	var pending []cleanPathSnap
	cursors := make([]reclaimCursor, 0, len(snaps))
	for _, snap := range snaps {
		if oldestSnapDay(snap) == "" {
			if snap.complete {
				c := idx.startReclaimCursor(snap)
				if c.peek() != nil {
					cursors = append(cursors, c)
				}
			}
			continue
		}
		pending = append(pending, snap)
	}

	h := cleanSegHeap{}
	for i := range cursors {
		seg := cursors[i].peek()
		if seg == nil {
			continue
		}
		h = append(h, cleanSegItem{
			ref: recordcleaner.SegmentRef{
				PathName: cursors[i].pathName,
				Fpath:    seg.Fpath(),
				Start:    seg.Start,
			},
			cursor: i,
		})
	}
	heap.Init(&h)
	idx.promotePending(&pending, &cursors, &h, minSnapDay(pending))

	out := make([]recordcleaner.SegmentRef, 0, min(limit*2, 1024))
	pressure := 0
	maxOut := limit * 8
	if maxOut < 256 {
		maxOut = 256
	}
	for pressure < limit && len(out) < maxOut {
		if h.Len() == 0 {
			next := minSnapDay(pending)
			if next == "" {
				break
			}
			n := len(cursors)
			idx.promotePending(&pending, &cursors, &h, next)
			if len(cursors) == n && h.Len() == 0 {
				continue
			}
			continue
		}
		item := heap.Pop(&h).(cleanSegItem)
		out = append(out, item.ref)
		if cleanPathUnderRoot(item.ref.Fpath, absDisk) {
			pressure++
		}
		cursors[item.cursor].pop()
		if len(cursors[item.cursor].segs) == 0 && len(cursors[item.cursor].days) > 0 {
			idx.promotePending(&pending, &cursors, &h, cursors[item.cursor].days[0])
		}
		idx.fillReclaimCursor(&cursors[item.cursor])
		if next := cursors[item.cursor].peek(); next != nil {
			heap.Push(&h, cleanSegItem{
				ref: recordcleaner.SegmentRef{
					PathName: cursors[item.cursor].pathName,
					Fpath:    next.Fpath(),
					Start:    next.Start,
				},
				cursor: item.cursor,
			})
		}
	}
	return out, true
}

// Flush implements recordcleaner.SegmentLister.
func (idx *Index) Flush() {
	if idx == nil {
		return
	}
	idx.mutex.Lock()
	if len(idx.metaDirty) == 0 {
		idx.mutex.Unlock()
		return
	}
	names := make([]string, 0, len(idx.metaDirty))
	for name := range idx.metaDirty {
		names = append(names, name)
	}
	idx.metaDirty = nil
	idx.mutex.Unlock()
	for _, name := range names {
		idx.writeMeta(name)
	}
}

func (idx *Index) cleanSnaps(storageName string) []cleanPathSnap {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	if idx.pathConfs == nil {
		return nil
	}
	out := make([]cleanPathSnap, 0)
	seen := make(map[string]struct{})
	for pathName, pe := range idx.paths {
		pathConf, _, err := conf.FindPathConf(idx.pathConfs, pathName)
		if err != nil || pathConf == nil || pathConf.Storage != storageName {
			continue
		}
		seen[pathName] = struct{}{}
		snap := cleanPathSnap{name: pathName}
		if pe != nil {
			snap.complete = pe.complete
			snap.layouts = append([]dvrPathLayout(nil), pe.allLayouts()...)
			for _, d := range pe.days {
				if d.Date != "" && d.NSeg > 0 {
					snap.days = append(snap.days, d.Date)
				}
			}
		}
		if len(snap.layouts) == 0 && pathConf != nil {
			snap.layouts = makeDvrLayouts(pathConf, pathName)
		}
		out = append(out, snap)
	}
	for pathName, pathConf := range idx.pathConfs {
		if pathConf == nil || pathConf.Storage != storageName {
			continue
		}
		if _, ok := seen[pathName]; ok {
			continue
		}
		if pathConf.Regexp != nil {
			continue
		}
		out = append(out, cleanPathSnap{
			name:    pathName,
			layouts: makeDvrLayouts(pathConf, pathName),
		})
	}
	return out
}

func oldestSnapDay(snap cleanPathSnap) string {
	if len(snap.days) == 0 {
		return ""
	}
	return snap.days[0]
}

func minSnapDay(snaps []cleanPathSnap) string {
	minDay := ""
	for _, snap := range snaps {
		d := oldestSnapDay(snap)
		if d == "" {
			continue
		}
		if minDay == "" || d < minDay {
			minDay = d
		}
	}
	return minDay
}

func (idx *Index) startReclaimCursor(snap cleanPathSnap) reclaimCursor {
	c := reclaimCursor{
		pathName: snap.name,
		days:     append([]string(nil), snap.days...),
		layouts:  snap.layouts,
	}
	if len(c.days) == 0 && snap.complete {
		c.segs = idx.ramSegsOldest(c.pathName)
		return c
	}
	idx.fillReclaimCursor(&c)
	return c
}

func (idx *Index) promotePending(pending *[]cleanPathSnap, cursors *[]reclaimCursor, h *cleanSegHeap, dayLE string) {
	if pending == nil || cursors == nil || h == nil || dayLE == "" {
		return
	}
	rest := make([]cleanPathSnap, 0, len(*pending))
	for _, snap := range *pending {
		d := oldestSnapDay(snap)
		if d == "" || d > dayLE {
			rest = append(rest, snap)
			continue
		}
		c := idx.startReclaimCursor(snap)
		if c.peek() == nil {
			continue
		}
		ci := len(*cursors)
		*cursors = append(*cursors, c)
		heap.Push(h, cleanSegItem{
			ref: recordcleaner.SegmentRef{
				PathName: c.pathName,
				Fpath:    c.peek().Fpath(),
				Start:    c.peek().Start,
			},
			cursor: ci,
		})
	}
	*pending = rest
}

type reclaimCursor struct {
	pathName string
	days     []string
	layouts  []dvrPathLayout
	segs     []*IndexedSegment
}

func (c *reclaimCursor) peek() *IndexedSegment {
	if c == nil || len(c.segs) == 0 {
		return nil
	}
	return c.segs[0]
}

func (c *reclaimCursor) pop() *IndexedSegment {
	if c == nil || len(c.segs) == 0 {
		return nil
	}
	s := c.segs[0]
	c.segs = c.segs[1:]
	return s
}

func (idx *Index) ramSegsOldest(pathName string) []*IndexedSegment {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	pe := idx.paths[pathName]
	if pe == nil || len(pe.segments) == 0 {
		return nil
	}
	out := make([]*IndexedSegment, len(pe.segments))
	copy(out, pe.segments)
	return out
}

func (idx *Index) fillReclaimCursor(c *reclaimCursor) {
	for c != nil && len(c.segs) == 0 && len(c.days) > 0 {
		day := c.days[0]
		c.days = c.days[1:]
		c.segs = idx.daySegsForClean(c.pathName, day)
	}
}

func (idx *Index) daySegsForClean(pathName, day string) []*IndexedSegment {
	if day == "" {
		return nil
	}
	segs := idx.segsForDay(pathName, day)
	if len(segs) > 0 {
		return segs
	}
	return idx.listDaySegs(pathName, day)
}

func (idx *Index) listDaySegs(pathName, day string) []*IndexedSegment {
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	var layouts []dvrPathLayout
	if pe != nil {
		layouts = append([]dvrPathLayout(nil), pe.allLayouts()...)
	}
	idx.mutex.RUnlock()
	if len(layouts) == 0 {
		idx.mutex.RLock()
		if idx.pathConfs != nil {
			if pathConf, _, err := conf.FindPathConf(idx.pathConfs, pathName); err == nil && pathConf != nil {
				layouts = makeDvrLayouts(pathConf, pathName)
			}
		}
		idx.mutex.RUnlock()
	}
	var out []*IndexedSegment
	for _, l := range layouts {
		out = append(out, idx.listLayoutDaySegs(pathName, l, day)...)
	}
	if len(out) > 1 {
		sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	}
	return out
}

func (idx *Index) listLayoutDaySegs(pathName string, l dvrPathLayout, day string) []*IndexedSegment {
	if l.common == "" || day == "" {
		return nil
	}
	dir := l.common
	if l.dateDir {
		dir = filepath.Join(l.common, day)
	}
	ents := idx.cachedDir(dir)
	var out []*IndexedSegment
	for _, e := range ents {
		if e.isDir || isDvrIndexFile(e.name) {
			continue
		}
		fpath := filepath.Join(dir, e.name)
		start, ok := idx.decodeStart(pathName, fpath)
		if !ok || dvrDayDate(start) != day {
			continue
		}
		out = append(out, &IndexedSegment{
			Rel:    segmentRelFast(l.common, fpath),
			Start:  start,
			common: l.common,
		})
	}
	return out
}

func (idx *Index) cachedDir(dir string) []cachedDirEnt {
	if idx == nil || dir == "" {
		return nil
	}
	idx.dirListMu.Lock()
	if idx.dirListCache != nil {
		if ents, ok := idx.dirListCache[dir]; ok {
			idx.dirListMu.Unlock()
			return ents
		}
	}
	idx.dirListMu.Unlock()

	raw, err := readDir(dir)
	if err != nil {
		idx.dirListMu.Lock()
		if idx.dirListCache == nil {
			idx.dirListCache = make(map[string][]cachedDirEnt)
		}
		idx.dirListCache[dir] = []cachedDirEnt{}
		idx.dirListMu.Unlock()
		return nil
	}
	ents := make([]cachedDirEnt, 0, len(raw))
	for _, e := range raw {
		ents = append(ents, cachedDirEnt{name: e.Name(), isDir: e.IsDir()})
	}
	idx.dirListMu.Lock()
	if idx.dirListCache == nil {
		idx.dirListCache = make(map[string][]cachedDirEnt)
	}
	idx.dirListCache[dir] = ents
	idx.dirListMu.Unlock()
	return ents
}

func (idx *Index) dropDirCacheName(dir, name string) {
	if dir == "" || name == "" {
		return
	}
	idx.dirListMu.Lock()
	defer idx.dirListMu.Unlock()
	ents := idx.dirListCache[dir]
	for i, e := range ents {
		if e.name == name {
			idx.dirListCache[dir] = append(ents[:i], ents[i+1:]...)
			return
		}
	}
}

func (idx *Index) dropDirCache(dir string) {
	if dir == "" {
		return
	}
	idx.dirListMu.Lock()
	delete(idx.dirListCache, dir)
	idx.dirListMu.Unlock()
}

type cleanSegItem struct {
	ref    recordcleaner.SegmentRef
	cursor int
}

type cleanSegHeap []cleanSegItem

func (h cleanSegHeap) Len() int { return len(h) }
func (h cleanSegHeap) Less(i, j int) bool {
	if h[i].ref.Start.Equal(h[j].ref.Start) {
		if h[i].ref.PathName != h[j].ref.PathName {
			return h[i].ref.PathName < h[j].ref.PathName
		}
		return h[i].ref.Fpath < h[j].ref.Fpath
	}
	return h[i].ref.Start.Before(h[j].ref.Start)
}
func (h cleanSegHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *cleanSegHeap) Push(x any)   { *h = append(*h, x.(cleanSegItem)) }
func (h *cleanSegHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

func cleanPathUnderRoot(fpath, root string) bool {
	if fpath == "" || root == "" {
		return false
	}
	absFile, err := filepath.Abs(fpath)
	if err != nil {
		absFile = filepath.Clean(fpath)
	}
	if absFile == root {
		return true
	}
	sep := string(os.PathSeparator)
	prefix := root
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	return strings.HasPrefix(absFile, prefix)
}
