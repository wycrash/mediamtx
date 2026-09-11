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
	SegmentsBefore(pathName string, end time.Time) ([]SegmentRef, bool)
	// OldestOnDisk returns up to limit oldest segments stored under diskRoot
	// for paths that use the named storage pool.
	OldestOnDisk(storageName, diskRoot string, limit int) ([]SegmentRef, bool)
}
