package compatapi

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recorder"
)

const chunkPackFlushEvery = 32

type chunkJob struct {
	pathName string
	fpath    string
}

type packDirtyKey struct {
	path string
	day  string
}

func applyRecorderParts(meta *fmp4SegMeta, parts []recorder.SegmentPart, chunkDur time.Duration) int {
	if meta == nil || len(parts) == 0 {
		return 0
	}
	media := make([]fmp4MediaPart, len(parts))
	for i, p := range parts {
		media[i] = fmp4MediaPart{
			Off:      p.Off,
			Len:      p.Len,
			Duration: p.Duration,
			PTSStart: p.DTSStart,
			HasIDR:   p.HasIDR,
		}
	}
	meta.MoofCount = uint32(len(media))
	last := media[len(media)-1]
	meta.Size = last.Off + last.Len
	if !shouldSliceFMP4(meta.Duration, chunkDur) {
		return 0
	}
	chunks := groupHLSChunks(media, chunkDur)
	if len(chunks) < 2 {
		return 0
	}
	meta.Chunks = chunks
	return len(chunks)
}

func needsHLSChunks(seg *IndexedSegment, chunkDur time.Duration) bool {
	if seg == nil || !seg.fmp4.Ready || len(seg.fmp4.Chunks) > 1 {
		return false
	}
	if !shouldSliceFMP4(seg.fmp4.Duration, chunkDur) {
		return false
	}
	return strings.HasSuffix(seg.Name(), ".mp4")
}

func (idx *Index) chunkDurationOf(pathName string) time.Duration {
	if idx == nil || idx.pathConfs == nil {
		return 0
	}
	pathConf, _, err := conf.FindPathConf(idx.pathConfs, pathName)
	if err != nil || pathConf == nil {
		return 0
	}
	return time.Duration(pathConf.RecordHlsChunkDuration)
}

func (idx *Index) pathHash(pathName string, pe *pathIndex) uint64 {
	if pe != nil && pe.persist != nil && pe.persist.hash != 0 {
		return pe.persist.hash
	}
	if idx == nil || idx.pathConfs == nil {
		return 0
	}
	pathConf, _, err := conf.FindPathConf(idx.pathConfs, pathName)
	if err != nil || pathConf == nil {
		return 0
	}
	return dvrIndexHash(pathConf, pathName)
}

func (idx *Index) enqueueChunkJob(pathName, fpath string) {
	if idx == nil || pathName == "" || fpath == "" || idx.chunkQ == nil {
		return
	}
	job := chunkJob{pathName: pathName, fpath: fpath}
	select {
	case idx.chunkQ <- job:
		idx.wakeChunkScan()
	default:
	}
}

func (idx *Index) wakeChunkScan() {
	if idx == nil {
		return
	}
	idx.chunkMu.Lock()
	idx.chunkIdle = false
	idx.chunkGen++
	idx.chunkMu.Unlock()
}

// RunChunkFill inspects closed fMP4 files off the record path and stores HLS
// chunk ranges in RAM and in the day pack. Until a file is filled, playlists
// serve it as a single URI.
func (idx *Index) RunChunkFill(stop <-chan struct{}) {
	if idx == nil {
		return
	}
	filled := 0
	for {
		if stopped(stop) {
			idx.flushDirtyPacks()
			return
		}
		job, ok := idx.takeChunkJob(stop)
		if !ok {
			idx.flushDirtyPacks()
			return
		}
		if job.pathName == "" {
			idx.flushDirtyPacks()
			continue
		}
		idx.fillChunkJob(job)
		filled++
		if filled%chunkPackFlushEvery == 0 {
			idx.flushDirtyPacks()
		}
		select {
		case <-stop:
			idx.flushDirtyPacks()
			return
		case <-time.After(reconcileInspectPause):
		}
	}
}

func (idx *Index) takeChunkJob(stop <-chan struct{}) (chunkJob, bool) {
	select {
	case <-stop:
		return chunkJob{}, false
	case job := <-idx.chunkQ:
		return job, true
	default:
	}
	idx.chunkMu.Lock()
	idle := idx.chunkIdle
	gen := idx.chunkGen
	idx.chunkMu.Unlock()
	if !idle {
		if job, ok := idx.scanOneMissing(); ok {
			return job, true
		}
		idx.chunkMu.Lock()
		if idx.chunkGen == gen {
			idx.chunkIdle = true
		}
		idx.chunkMu.Unlock()
	}
	select {
	case <-stop:
		return chunkJob{}, false
	case job := <-idx.chunkQ:
		return job, true
	case <-time.After(2 * time.Second):
		return chunkJob{pathName: ""}, true
	}
}

func (idx *Index) scanOneMissing() (chunkJob, bool) {
	idx.mutex.RLock()
	names := make([]string, 0, len(idx.paths))
	for name := range idx.paths {
		names = append(names, name)
	}
	idx.mutex.RUnlock()
	if len(names) == 0 {
		return chunkJob{}, false
	}
	sort.Strings(names)

	idx.chunkMu.Lock()
	cursorPath := idx.chunkPath
	cursorSeg := idx.chunkSeg
	idx.chunkMu.Unlock()

	startIdx := 0
	if cursorPath != "" {
		i := sort.SearchStrings(names, cursorPath)
		if i < len(names) {
			startIdx = i
		}
	}

	for round := 0; round < 2; round++ {
		for _, name := range names[startIdx:] {
			chunkDur := idx.chunkDurationOf(name)
			if chunkDur <= 0 {
				continue
			}
			idx.mutex.RLock()
			pe := idx.paths[name]
			job := chunkJob{}
			found := false
			nextSeg := 0
			if pe != nil {
				start := 0
				if name == cursorPath {
					start = cursorSeg
				}
				for i := start; i < len(pe.segments); i++ {
					seg := pe.segments[i]
					if !needsHLSChunks(seg, chunkDur) {
						continue
					}
					job = chunkJob{pathName: name, fpath: seg.Fpath()}
					nextSeg = i + 1
					found = true
					break
				}
			}
			idx.mutex.RUnlock()
			if found {
				idx.chunkMu.Lock()
				idx.chunkPath = name
				idx.chunkSeg = nextSeg
				idx.chunkMu.Unlock()
				return job, true
			}
			cursorPath = ""
			cursorSeg = 0
		}
		startIdx = 0
		cursorPath = ""
		cursorSeg = 0
	}
	idx.chunkMu.Lock()
	idx.chunkPath = ""
	idx.chunkSeg = 0
	idx.chunkMu.Unlock()
	return chunkJob{}, false
}

func (idx *Index) fillChunkJob(job chunkJob) {
	if idx == nil || job.pathName == "" || job.fpath == "" {
		return
	}
	chunkDur := idx.chunkDurationOf(job.pathName)
	if chunkDur <= 0 {
		return
	}

	idx.mutex.RLock()
	pe := idx.paths[job.pathName]
	var seg *IndexedSegment
	if pe != nil {
		seg = pe.byName[filepath.Base(job.fpath)]
	}
	skip := seg == nil || !needsHLSChunks(seg, chunkDur)
	idx.mutex.RUnlock()
	if skip {
		return
	}

	t0 := time.Now()
	chunks, size, err := inspectHLSChunks(job.fpath, chunkDur)
	idx.debug.noteInspect(time.Since(t0))
	if err != nil || len(chunks) < 2 {
		return
	}

	idx.mutex.Lock()
	pe = idx.paths[job.pathName]
	if pe == nil {
		idx.mutex.Unlock()
		return
	}
	seg = pe.byName[filepath.Base(job.fpath)]
	if seg == nil || !needsHLSChunks(seg, chunkDur) {
		idx.mutex.Unlock()
		return
	}
	seg.fmp4.Chunks = chunks
	seg.fmp4.Size = size
	day := dvrDayDate(seg.Start)
	idx.mutex.Unlock()

	idx.markPackDirty(job.pathName, day)
}

func inspectHLSChunks(fpath string, chunkDur time.Duration) ([]hlsMediaChunk, int64, error) {
	fi, err := os.Stat(fpath)
	if err != nil {
		return nil, 0, err
	}
	parts, err := loadFMP4MediaParts(fpath)
	if err != nil {
		return nil, fi.Size(), err
	}
	chunks := groupHLSChunks(parts, chunkDur)
	if len(chunks) < 2 {
		return nil, fi.Size(), nil
	}
	return chunks, fi.Size(), nil
}

func (idx *Index) markPackDirty(pathName, day string) {
	if idx == nil || pathName == "" || day == "" {
		return
	}
	idx.chunkMu.Lock()
	if idx.packDirty == nil {
		idx.packDirty = make(map[packDirtyKey]struct{})
	}
	idx.packDirty[packDirtyKey{path: pathName, day: day}] = struct{}{}
	idx.chunkMu.Unlock()
}

func (idx *Index) flushDirtyPacks() {
	if idx == nil {
		return
	}
	idx.chunkMu.Lock()
	if len(idx.packDirty) == 0 {
		idx.chunkMu.Unlock()
		return
	}
	keys := make([]packDirtyKey, 0, len(idx.packDirty))
	for k := range idx.packDirty {
		keys = append(keys, k)
	}
	idx.packDirty = make(map[packDirtyKey]struct{})
	idx.chunkMu.Unlock()
	for _, k := range keys {
		idx.writeDayPack(k.path, k.day)
	}
}

func (idx *Index) writeDayPack(pathName, day string) {
	if idx == nil || pathName == "" || day == "" {
		return
	}
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	if pe == nil {
		idx.mutex.RUnlock()
		return
	}
	hash := idx.pathHash(pathName, pe)
	var segs []*IndexedSegment
	commons := map[string][]*IndexedSegment{}
	for _, seg := range pe.segments {
		if seg == nil || dvrDayDate(seg.Start) != day || !seg.fmp4.Ready {
			continue
		}
		segs = append(segs, seg)
		commons[seg.common] = append(commons[seg.common], seg)
	}
	layouts := append([]dvrPathLayout(nil), pe.allLayouts()...)
	idx.mutex.RUnlock()
	if hash == 0 || len(segs) == 0 {
		return
	}
	dayUnix := dvrDayUnix(day)
	if dayUnix == 0 {
		return
	}
	for _, l := range layouts {
		daySegs := commons[l.common]
		if len(daySegs) == 0 && l.common == "" {
			daySegs = segs
		}
		p := packFromSegs(hash, dayUnix, daySegs)
		if p == nil {
			continue
		}
		_ = writeDayPackFile(l.dayPack(day), p)
	}
}

func (idx *Index) overlayDayPack(l dvrPathLayout, day string, hash uint64, segs []*IndexedSegment) {
	if len(segs) == 0 || day == "" || hash == 0 {
		return
	}
	path := l.dayPack(day)
	var (
		data []byte
		err  error
	)
	if idx != nil {
		data, err = idx.readIndexFile("read-pack", path)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return
	}
	p, err := decodeDvrPack(data, hash)
	if err != nil || p == nil {
		return
	}
	overlayPackChunks(segs, p)
}

func (idx *Index) hlsSliceRange(pathName, fpath string, off int64, n int, fileSize int64) (int64, int64, bool) {
	if idx == nil || fpath == "" {
		return 0, 0, false
	}
	name := filepath.Base(fpath)
	idx.mutex.RLock()
	pe := idx.paths[pathName]
	var seg *IndexedSegment
	if pe != nil {
		seg = pe.byName[name]
	}
	if seg == nil || len(seg.fmp4.Chunks) < 2 {
		idx.mutex.RUnlock()
		return 0, 0, false
	}
	size := seg.fmp4.Size
	chunks := seg.fmp4.Chunks
	idx.mutex.RUnlock()
	if size > 0 && fileSize > 0 && size != fileSize {
		idx.mutex.Lock()
		if pe := idx.paths[pathName]; pe != nil {
			if s := pe.byName[name]; s != nil && s.fmp4.Size > 0 && s.fmp4.Size != fileSize {
				s.fmp4.Chunks = nil
				s.fmp4.Size = 0
			}
		}
		idx.mutex.Unlock()
		return 0, 0, false
	}
	return hlsChunkByteRange(chunks, off, n, fileSize)
}
