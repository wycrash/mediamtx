package compatapi

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

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
}

// Index keeps an in-memory inventory of recording segments.
type Index struct {
	mutex     sync.RWMutex
	paths     map[string]*pathIndex
	pathConfs map[string]*conf.Path
}

// NewIndex allocates an Index.
func NewIndex() *Index {
	return &Index{
		paths: make(map[string]*pathIndex),
	}
}

// ReloadPathConfs updates path configuration used for decoding / durations.
func (idx *Index) ReloadPathConfs(pathConfs map[string]*conf.Path) {
	idx.mutex.Lock()
	defer idx.mutex.Unlock()
	idx.pathConfs = pathConfs
	for name, pe := range idx.paths {
		if pathConf, _, err := conf.FindPathConf(pathConfs, name); err == nil {
			dur := time.Duration(pathConf.RecordSegmentDuration)
			if pe.segmentDuration != dur {
				pe.segmentDuration = dur
				pe.rangesOK = false
			}
		}
	}
}

// LoadFromDisk scans existing recordings into memory.
// fMP4 files are inspected once here (duration, moof count, tracks) so archive
// playlists can be built without reopening files on each request.
func (idx *Index) LoadFromDisk(pathConfs map[string]*conf.Path) {
	idx.mutex.Lock()
	idx.pathConfs = pathConfs
	idx.paths = make(map[string]*pathIndex)
	idx.mutex.Unlock()

	type pending struct {
		pathName string
		format   conf.RecordFormat
		fpath    string
		start    time.Time
	}

	var pendingSegs []pending
	pathNames := recordstore.FindAllPathsWithSegments(pathConfs)
	for _, pathName := range pathNames {
		pathConf, _, err := conf.FindPathConf(pathConfs, pathName)
		if err != nil {
			continue
		}
		segments, err := recordstore.FindSegments(pathConf, pathName, nil, nil)
		if err != nil {
			continue
		}
		for _, seg := range segments {
			pendingSegs = append(pendingSegs, pending{
				pathName: pathName,
				format:   pathConf.RecordFormat,
				fpath:    seg.Fpath,
				start:    seg.Start,
			})
		}
	}

	fpaths := make([]string, len(pendingSegs))
	for i, p := range pendingSegs {
		if p.format == conf.RecordFormatFMP4 {
			fpaths[i] = p.fpath
		}
	}
	metas := inspectFMP4Segments(fpaths)

	for i, p := range pendingSegs {
		idx.Add(p.pathName, p.fpath, p.start)
		if p.format == conf.RecordFormatFMP4 && metas[i].Ready {
			idx.SetFMP4Meta(p.pathName, p.fpath, metas[i])
			metas[i].Tracks = nil
		}
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
		if fmp4TracksCompatible(existing, tracks) {
			return existing
		}
	}
	pe.internedTracks = append(pe.internedTracks, tracks)
	return tracks
}

// Add inserts or updates a segment.
func (idx *Index) Add(pathName, fpath string, start time.Time) {
	if pathName == "" || fpath == "" || start.IsZero() {
		return
	}

	idx.mutex.Lock()
	defer idx.mutex.Unlock()

	pe := idx.ensurePathLocked(pathName)
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

	// insert sorted by Start
	i := sort.Search(len(pe.segments), func(i int) bool {
		return !pe.segments[i].Start.Before(start)
	})
	pe.segments = append(pe.segments, nil)
	copy(pe.segments[i+1:], pe.segments[i:])
	pe.segments[i] = seg
	pe.rangesOK = false
}

// AddFromPath decodes start time from fpath using path config and adds the segment.
func (idx *Index) AddFromPath(pathName, fpath string) {
	start, ok := idx.decodeStart(pathName, fpath)
	if !ok {
		return
	}
	idx.Add(pathName, fpath, start)
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
