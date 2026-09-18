package recordcleaner

import "time"

// SegmentRef is a recording segment candidate for deletion.
type SegmentRef struct {
	PathName string
	Fpath    string
	Start    time.Time
}

// SegmentLister provides deletion candidates without a full filesystem walk.
// ok=false means the caller must fall back to WalkDir / FindSegments.
type SegmentLister interface {
	// PathNames returns indexed recording paths (typically complete ones).
	PathNames() ([]string, bool)
	// SegmentsBefore returns segments with Start <= end for a path.
	// Uses on-disk day journals; RAM may only hold the current day.
	SegmentsBefore(pathName string, end time.Time) ([]SegmentRef, bool)
	// ReclaimCandidates returns a chronological prefix of segments to delete
	// so that `limit` of them lie under diskRoot. Segments on other disks of
	// the same storage are included when they are older (round-robin gap
	// prevention). Source is on-disk day indexes, not pinned RAM.
	ReclaimCandidates(storageName, diskRoot string, limit int) ([]SegmentRef, bool)
	// Flush persists dirty index meta after a delete batch.
	Flush()
}
