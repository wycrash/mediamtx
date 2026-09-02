package compatapi

import (
	"strconv"
	"time"

	"github.com/bluenviron/mediamtx/internal/recordstore"
)

// RecordingRange is a contiguous archive timespan.
type RecordingRange struct {
	From     int64 `json:"from"`
	Duration int64 `json:"duration"`
}

// RecordingStatus is the Flussonic-compatible status payload entry.
type RecordingStatus struct {
	Stream string           `json:"stream"`
	Ranges []RecordingRange `json:"ranges"`
}

// DVRRange is a Flussonic ranges.json timespan.
type DVRRange struct {
	Duration int64 `json:"duration"`
	From     int64 `json:"from"`
	OpenedAt int64 `json:"opened_at"`
	ClosedAt int64 `json:"closed_at"`
}

// RangesTiming is the Flussonic collection timing payload.
type RangesTiming struct {
	Select int `json:"select"`
	Sort   int `json:"sort"`
	Filter int `json:"filter"`
	Limit  int `json:"limit"`
}

// RangesJSON is the Flussonic ranges.json response.
type RangesJSON struct {
	EstimatedCount int          `json:"estimated_count"`
	Timing         RangesTiming `json:"timing"`
	Ranges         []DVRRange   `json:"ranges"`
}

type rangesQuery struct {
	openedAtGte *int64
	openedAtGt  *int64
	openedAtLte *int64
	openedAtLt  *int64
	closedAtGte *int64
	closedAtGt  *int64
	closedAtLte *int64
	closedAtLt  *int64
	limit       int
	resolution  int64
}

func (r RecordingRange) closedAt() int64 {
	return r.From + r.Duration
}

func (r RecordingRange) toDVR() DVRRange {
	return DVRRange{
		Duration: r.Duration,
		From:     r.From,
		OpenedAt: r.From,
		ClosedAt: r.closedAt(),
	}
}

func parseUnixQuery(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	// Flussonic accepts seconds or milliseconds.
	if v >= 1e12 {
		v /= 1000
	}
	return v, true
}

func parseRangesQuery(q func(string) string) rangesQuery {
	out := rangesQuery{}
	if v, ok := parseUnixQuery(q("opened_at_gte")); ok {
		out.openedAtGte = &v
	}
	if v, ok := parseUnixQuery(q("opened_at_gt")); ok {
		out.openedAtGt = &v
	}
	if v, ok := parseUnixQuery(q("opened_at_lte")); ok {
		out.openedAtLte = &v
	}
	if v, ok := parseUnixQuery(q("opened_at_lt")); ok {
		out.openedAtLt = &v
	}
	if v, ok := parseUnixQuery(q("closed_at_gte")); ok {
		out.closedAtGte = &v
	}
	if v, ok := parseUnixQuery(q("closed_at_gt")); ok {
		out.closedAtGt = &v
	}
	if v, ok := parseUnixQuery(q("closed_at_lte")); ok {
		out.closedAtLte = &v
	}
	if v, ok := parseUnixQuery(q("closed_at_lt")); ok {
		out.closedAtLt = &v
	}
	if raw := q("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			out.limit = n
		}
	}
	if v, ok := parseUnixQuery(q("resolution")); ok {
		out.resolution = v
	}
	return out
}

func mergeRangesByResolution(ranges []RecordingRange, resolution int64) []RecordingRange {
	if resolution <= 0 || len(ranges) < 2 {
		return ranges
	}

	out := make([]RecordingRange, 0, len(ranges))
	cur := ranges[0]
	for _, next := range ranges[1:] {
		gap := next.From - cur.closedAt()
		if gap >= 0 && gap <= resolution {
			cur.Duration = next.closedAt() - cur.From
			continue
		}
		out = append(out, cur)
		cur = next
	}
	out = append(out, cur)
	return out
}

func rangeMatchesQuery(r RecordingRange, q rangesQuery) bool {
	opened := r.From
	closed := r.closedAt()
	if q.openedAtGte != nil && opened < *q.openedAtGte {
		return false
	}
	if q.openedAtGt != nil && opened <= *q.openedAtGt {
		return false
	}
	if q.openedAtLte != nil && opened > *q.openedAtLte {
		return false
	}
	if q.openedAtLt != nil && opened >= *q.openedAtLt {
		return false
	}
	if q.closedAtGte != nil && closed < *q.closedAtGte {
		return false
	}
	if q.closedAtGt != nil && closed <= *q.closedAtGt {
		return false
	}
	if q.closedAtLte != nil && closed > *q.closedAtLte {
		return false
	}
	if q.closedAtLt != nil && closed >= *q.closedAtLt {
		return false
	}
	return true
}

func buildRangesJSON(ranges []RecordingRange, q rangesQuery) RangesJSON {
	ranges = mergeRangesByResolution(ranges, q.resolution)

	filtered := make([]RecordingRange, 0, len(ranges))
	for _, r := range ranges {
		if rangeMatchesQuery(r, q) {
			filtered = append(filtered, r)
		}
	}

	out := RangesJSON{
		EstimatedCount: len(filtered),
		Timing:         RangesTiming{},
		Ranges:         make([]DVRRange, 0, len(filtered)),
	}
	if q.limit > 0 && len(filtered) > q.limit {
		filtered = filtered[:q.limit]
	}
	for _, r := range filtered {
		out.Ranges = append(out.Ranges, r.toDVR())
	}
	return out
}

type timedSeg struct {
	Start    time.Time
	Duration time.Duration
}

func rangeMergeTolerance(nominal time.Duration) time.Duration {
	t := 2 * time.Second
	if nominal > 0 && nominal/10 > t {
		t = nominal / 10
	}
	if t > 5*time.Second {
		t = 5 * time.Second
	}
	return t
}

func segDurationCap(nominal time.Duration) time.Duration {
	if nominal <= 0 {
		nominal = time.Hour
	}
	return nominal + rangeMergeTolerance(nominal)
}

// trustedSegDuration is the duration used for recording_status / index ranges.
// A known file duration wins when it is at most a segment plus merge tolerance.
// Otherwise a following file within that window is chained. Larger gaps stay holes
// (round-robin files on one disk are typically ~2× segment apart).
func trustedSegDuration(stored, nextDelta, nominal time.Duration) time.Duration {
	if nominal <= 0 {
		nominal = time.Hour
	}
	cap := segDurationCap(nominal)
	if stored > 0 && stored <= cap {
		return stored
	}
	if nextDelta > 0 && nextDelta <= cap {
		return nextDelta
	}
	return nominal
}

// BuildRanges merges segments into contiguous ranges.
// segmentDuration is the fallback duration when a segment's real length is unknown.
func BuildRanges(segments []*recordstore.Segment, segmentDuration time.Duration) []RecordingRange {
	segs := make([]timedSeg, len(segments))
	for i, s := range segments {
		segs[i] = timedSeg{Start: s.Start, Duration: segmentDuration}
	}
	return buildRanges(segs, segmentDuration, time.Now())
}

func buildRanges(segs []timedSeg, nominal time.Duration, now time.Time) []RecordingRange {
	if len(segs) == 0 || nominal <= 0 {
		return []RecordingRange{}
	}

	tolerance := rangeMergeTolerance(nominal)

	var ranges []RecordingRange
	var curStart, curEnd time.Time
	started := false

	flush := func() {
		if !started {
			return
		}
		if curEnd.After(now) {
			curEnd = now
		}
		dur := curEnd.Sub(curStart)
		if dur >= time.Second {
			ranges = append(ranges, RecordingRange{
				From:     curStart.Unix(),
				Duration: int64(dur.Seconds()),
			})
		}
		started = false
	}

	for _, seg := range segs {
		dur := seg.Duration
		if dur <= 0 {
			dur = nominal
		}
		segEnd := seg.Start.Add(dur)
		if !started {
			curStart = seg.Start
			curEnd = segEnd
			started = true
			continue
		}

		gap := seg.Start.Sub(curEnd)
		if gap <= tolerance {
			if segEnd.After(curEnd) {
				curEnd = segEnd
			}
			continue
		}

		flush()
		curStart = seg.Start
		curEnd = segEnd
		started = true
	}
	flush()

	if ranges == nil {
		return []RecordingRange{}
	}
	return ranges
}
