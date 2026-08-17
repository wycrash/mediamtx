package compatapi

import (
	"fmt"
	"runtime"
	"sort"
	"unsafe"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

// IndexPathMemStats is a per-path slice of IndexMemStats.
type IndexPathMemStats struct {
	Name         string
	Segments     int
	WithTracks   int
	InternedSets int
	FpathBytes   int
	NameBytes    int
}

// IndexMemStats is a retained-memory breakdown of the recording index.
type IndexMemStats struct {
	Paths             int
	Segments          int
	FMP4Ready         int
	SegsWithTracks    int
	TrackPtrs         int
	UniqueTrackPtrs   int
	InternedSets      int
	FpathBytes        int
	NameBytes         int
	CodecPayloadBytes int
	InitCacheEntries  int
	EstLiveBytes      int64
	PathsDetail       []IndexPathMemStats
}

type procMemStats struct {
	heapAlloc   uint64
	heapInuse   uint64
	heapSys     uint64
	sys         uint64
	heapObjects uint64
	goroutines  int
}

func readProcMem() procMemStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return procMemStats{
		heapAlloc:   m.HeapAlloc,
		heapInuse:   m.HeapInuse,
		heapSys:     m.HeapSys,
		sys:         m.Sys,
		heapObjects: m.HeapObjects,
		goroutines:  runtime.NumGoroutine(),
	}
}

func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}

func (p procMemStats) logLine() string {
	return fmt.Sprintf("heap_alloc=%s heap_inuse=%s heap_sys=%s sys=%s heap_objects=%d goroutines=%d",
		formatBytes(p.heapAlloc),
		formatBytes(p.heapInuse),
		formatBytes(p.heapSys),
		formatBytes(p.sys),
		p.heapObjects,
		p.goroutines,
	)
}

// MemStats returns a retained-memory breakdown of the in-memory index.
func (idx *Index) MemStats() IndexMemStats {
	idx.mutex.RLock()
	defer idx.mutex.RUnlock()

	st := IndexMemStats{
		Paths:       len(idx.paths),
		PathsDetail: make([]IndexPathMemStats, 0, len(idx.paths)),
	}
	unique := make(map[uintptr]struct{})
	segSize := int64(unsafe.Sizeof(IndexedSegment{}))
	trackSize := int64(unsafe.Sizeof(fmp4.InitTrack{}))

	for name, pe := range idx.paths {
		ps := IndexPathMemStats{
			Name:         name,
			Segments:     len(pe.segments),
			InternedSets: len(pe.internedTracks),
		}
		st.Segments += len(pe.segments)
		st.InternedSets += len(pe.internedTracks)
		for _, seg := range pe.segments {
			st.FpathBytes += len(seg.Fpath)
			st.NameBytes += len(seg.Name)
			ps.FpathBytes += len(seg.Fpath)
			ps.NameBytes += len(seg.Name)
			if seg.fmp4.Ready {
				st.FMP4Ready++
			}
			if len(seg.fmp4.Tracks) == 0 {
				continue
			}
			st.SegsWithTracks++
			ps.WithTracks++
			st.TrackPtrs += len(seg.fmp4.Tracks)
			for _, tr := range seg.fmp4.Tracks {
				if tr == nil {
					continue
				}
				ptr := uintptr(unsafe.Pointer(tr))
				if _, ok := unique[ptr]; ok {
					continue
				}
				unique[ptr] = struct{}{}
				st.CodecPayloadBytes += codecPayloadBytes(tr.Codec)
			}
		}
		st.PathsDetail = append(st.PathsDetail, ps)
	}
	sort.Slice(st.PathsDetail, func(i, j int) bool {
		return st.PathsDetail[i].Name < st.PathsDetail[j].Name
	})

	st.UniqueTrackPtrs = len(unique)
	fmp4InitSizeCache.Range(func(_, _ any) bool {
		st.InitCacheEntries++
		return true
	})

	// Lower bound: structs + string bytes + unique track objects + codec blobs + map/slice pointers.
	st.EstLiveBytes = segSize*int64(st.Segments) +
		int64(st.FpathBytes+st.NameBytes+st.CodecPayloadBytes) +
		trackSize*int64(st.UniqueTrackPtrs) +
		int64(st.Segments)*48 + // byName map entry
		int64(st.InitCacheEntries)*64
	return st
}

func codecPayloadBytes(c mcodecs.Codec) int {
	switch v := c.(type) {
	case *mcodecs.H264:
		return len(v.SPS) + len(v.PPS)
	case *mcodecs.H265:
		return len(v.VPS) + len(v.SPS) + len(v.PPS)
	case *mcodecs.AV1:
		return len(v.SequenceHeader)
	case *mcodecs.MPEG4Video:
		return len(v.Config)
	default:
		return 0
	}
}
