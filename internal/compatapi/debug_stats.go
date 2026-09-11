package compatapi

import (
	"fmt"
	"sync/atomic"
	"time"
)

// indexDebugStats tracks hot-path work that can burn CPU/disk without HTTP traffic.
type indexDebugStats struct {
	segmentCreate   atomic.Uint64
	segmentComplete atomic.Uint64
	segmentRemove   atomic.Uint64
	writeMeta       atomic.Uint64
	writeMetaNs     atomic.Uint64
	persistUpsert   atomic.Uint64
	reconcilePath   atomic.Uint64
	reconcilePathNs atomic.Uint64
	inspectFMP4     atomic.Uint64
	inspectFMP4Ns   atomic.Uint64
	slowOps         atomic.Uint64

	lastCreateNs   atomic.Int64
	lastCompleteNs atomic.Int64
	lastRemoveNs   atomic.Int64
	lastMetaNs     atomic.Int64
}

func (s *indexDebugStats) snapshot() debugStatsSnapshot {
	if s == nil {
		return debugStatsSnapshot{}
	}
	metaN := s.writeMeta.Load()
	metaNs := s.writeMetaNs.Load()
	recN := s.reconcilePath.Load()
	recNs := s.reconcilePathNs.Load()
	insN := s.inspectFMP4.Load()
	insNs := s.inspectFMP4Ns.Load()
	return debugStatsSnapshot{
		SegmentCreate:     s.segmentCreate.Load(),
		SegmentComplete:   s.segmentComplete.Load(),
		SegmentRemove:     s.segmentRemove.Load(),
		WriteMeta:         metaN,
		WriteMetaAvgMs:    avgMs(metaNs, metaN),
		PersistUpsert:     s.persistUpsert.Load(),
		ReconcilePath:     recN,
		ReconcilePathAvgMs: avgMs(recNs, recN),
		InspectFMP4:       insN,
		InspectFMP4AvgMs:  avgMs(insNs, insN),
		SlowOps:           s.slowOps.Load(),
		LastCreate:        unixNsTime(s.lastCreateNs.Load()),
		LastComplete:      unixNsTime(s.lastCompleteNs.Load()),
		LastRemove:        unixNsTime(s.lastRemoveNs.Load()),
		LastWriteMeta:     unixNsTime(s.lastMetaNs.Load()),
	}
}

func (s *indexDebugStats) resetInterval() debugStatsSnapshot {
	snap := s.snapshot()
	if s == nil {
		return snap
	}
	s.segmentCreate.Store(0)
	s.segmentComplete.Store(0)
	s.segmentRemove.Store(0)
	s.writeMeta.Store(0)
	s.writeMetaNs.Store(0)
	s.persistUpsert.Store(0)
	s.reconcilePath.Store(0)
	s.reconcilePathNs.Store(0)
	s.inspectFMP4.Store(0)
	s.inspectFMP4Ns.Store(0)
	s.slowOps.Store(0)
	return snap
}

type debugStatsSnapshot struct {
	SegmentCreate      uint64
	SegmentComplete    uint64
	SegmentRemove      uint64
	WriteMeta          uint64
	WriteMetaAvgMs     float64
	PersistUpsert      uint64
	ReconcilePath      uint64
	ReconcilePathAvgMs float64
	InspectFMP4        uint64
	InspectFMP4AvgMs   float64
	SlowOps            uint64
	LastCreate         time.Time
	LastComplete       time.Time
	LastRemove         time.Time
	LastWriteMeta      time.Time
}

func (s debugStatsSnapshot) logLine() string {
	return fmt.Sprintf(
		"create=%d complete=%d remove=%d writeMeta=%d(avg=%.1fms) upsert=%d reconcilePath=%d(avg=%.1fms) inspect=%d(avg=%.1fms) slowOps=%d lastComplete=%s lastMeta=%s",
		s.SegmentCreate,
		s.SegmentComplete,
		s.SegmentRemove,
		s.WriteMeta,
		s.WriteMetaAvgMs,
		s.PersistUpsert,
		s.ReconcilePath,
		s.ReconcilePathAvgMs,
		s.InspectFMP4,
		s.InspectFMP4AvgMs,
		s.SlowOps,
		formatAgo(s.LastComplete),
		formatAgo(s.LastWriteMeta),
	)
}

func avgMs(totalNs, n uint64) float64 {
	if n == 0 {
		return 0
	}
	return float64(totalNs) / float64(n) / 1e6
}

func unixNsTime(ns int64) time.Time {
	if ns <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func formatAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Truncate(time.Second).String() + " ago"
}

func (s *indexDebugStats) noteCreate() {
	if s == nil {
		return
	}
	s.segmentCreate.Add(1)
	s.lastCreateNs.Store(time.Now().UnixNano())
}

func (s *indexDebugStats) noteComplete() {
	if s == nil {
		return
	}
	s.segmentComplete.Add(1)
	s.lastCompleteNs.Store(time.Now().UnixNano())
}

func (s *indexDebugStats) noteRemove() {
	if s == nil {
		return
	}
	s.segmentRemove.Add(1)
	s.lastRemoveNs.Store(time.Now().UnixNano())
}

func (s *indexDebugStats) noteUpsert() {
	if s == nil {
		return
	}
	s.persistUpsert.Add(1)
}

func (s *indexDebugStats) noteWriteMeta(d time.Duration) {
	if s == nil {
		return
	}
	s.writeMeta.Add(1)
	s.writeMetaNs.Add(uint64(d))
	s.lastMetaNs.Store(time.Now().UnixNano())
	if d >= 50*time.Millisecond {
		s.slowOps.Add(1)
	}
}

func (s *indexDebugStats) noteReconcilePath(d time.Duration) {
	if s == nil {
		return
	}
	s.reconcilePath.Add(1)
	s.reconcilePathNs.Add(uint64(d))
	if d >= 50*time.Millisecond {
		s.slowOps.Add(1)
	}
}

func (s *indexDebugStats) noteInspect(d time.Duration) {
	if s == nil {
		return
	}
	s.inspectFMP4.Add(1)
	s.inspectFMP4Ns.Add(uint64(d))
	if d >= 50*time.Millisecond {
		s.slowOps.Add(1)
	}
}

const slowOpLogThreshold = 100 * time.Millisecond
