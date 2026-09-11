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

// Ensure Index satisfies the cleaner segment source interface.
var _ recordcleaner.SegmentLister = (*Index)(nil)

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
func (idx *Index) SegmentsBefore(pathName string, end time.Time) ([]recordcleaner.SegmentRef, bool) {
	if idx == nil || pathName == "" || end.IsZero() {
		return nil, false
	}
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	pe := idx.paths[pathName]
	if pe == nil || !pe.complete {
		return nil, false
	}
	// pe.segments is sorted by Start ascending.
	n := sort.Search(len(pe.segments), func(i int) bool {
		return pe.segments[i].Start.After(end)
	})
	if n == 0 {
		return nil, true
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
	return out, true
}

// OldestOnDisk implements recordcleaner.SegmentLister.
func (idx *Index) OldestOnDisk(storageName, diskRoot string, limit int) ([]recordcleaner.SegmentRef, bool) {
	if idx == nil || storageName == "" || diskRoot == "" || limit <= 0 {
		return nil, false
	}
	absDisk, err := filepath.Abs(diskRoot)
	if err != nil {
		absDisk = filepath.Clean(diskRoot)
	}

	idx.mutex.RLock()
	defer idx.mutex.RUnlock()
	if len(idx.paths) == 0 || idx.pathConfs == nil {
		return nil, false
	}

	type pathCursor struct {
		name string
		segs []*IndexedSegment
		i    int
	}
	var cursors []pathCursor
	for pathName, pe := range idx.paths {
		if pe == nil {
			continue
		}
		pathConf, _, err := conf.FindPathConf(idx.pathConfs, pathName)
		if err != nil || pathConf == nil || pathConf.Storage != storageName {
			continue
		}
		if !pe.complete {
			// Incomplete path may hold older files than the index knows —
			// fall back to WalkDir so reclaim stays correct.
			return nil, false
		}
		cursors = append(cursors, pathCursor{name: pathName, segs: pe.segments})
	}
	if len(cursors) == 0 {
		return nil, false
	}

	h := cleanSegHeap{}
	for ci := range cursors {
		c := &cursors[ci]
		for c.i < len(c.segs) {
			seg := c.segs[c.i]
			c.i++
			if seg == nil {
				continue
			}
			fpath := seg.Fpath()
			if !cleanPathUnderRoot(fpath, absDisk) {
				continue
			}
			heap.Push(&h, cleanSegItem{
				ref: recordcleaner.SegmentRef{
					PathName: c.name,
					Fpath:    fpath,
					Start:    seg.Start,
				},
				cursor: ci,
			})
			break
		}
	}
	heap.Init(&h)

	out := make([]recordcleaner.SegmentRef, 0, min(limit, 1024))
	for len(out) < limit && h.Len() > 0 {
		item := heap.Pop(&h).(cleanSegItem)
		out = append(out, item.ref)
		c := &cursors[item.cursor]
		for c.i < len(c.segs) {
			seg := c.segs[c.i]
			c.i++
			if seg == nil {
				continue
			}
			fpath := seg.Fpath()
			if !cleanPathUnderRoot(fpath, absDisk) {
				continue
			}
			heap.Push(&h, cleanSegItem{
				ref: recordcleaner.SegmentRef{
					PathName: c.name,
					Fpath:    fpath,
					Start:    seg.Start,
				},
				cursor: item.cursor,
			})
			break
		}
	}
	return out, true
}

type cleanSegItem struct {
	ref    recordcleaner.SegmentRef
	cursor int
}

type cleanSegHeap []cleanSegItem

func (h cleanSegHeap) Len() int { return len(h) }
func (h cleanSegHeap) Less(i, j int) bool {
	if h[i].ref.Start.Equal(h[j].ref.Start) {
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
