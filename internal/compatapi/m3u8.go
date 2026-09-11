package compatapi

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

const maxArchiveDuration = 24 * time.Hour

func playlistGapThreshold(segmentDuration time.Duration) time.Duration {
	t := segmentDuration + segmentDuration/2
	if t < 15*time.Second {
		return 15 * time.Second
	}
	return t
}

// timeshiftLookback is how much DVR behind the delayed edge to expose.
// Keep a short Flussonic-style live window (~4 segments); a large floor (e.g. 90s)
// with 5s segments flooded the playlist and caused ExoPlayer stutter.
func timeshiftLookback(segmentDuration time.Duration) time.Duration {
	if segmentDuration <= 0 {
		segmentDuration = 5 * time.Second
	}
	lb := 4 * segmentDuration
	if lb < 20*time.Second {
		lb = 20 * time.Second
	}
	if lb > 6*time.Hour {
		lb = 6 * time.Hour
	}
	if lb > maxArchiveDuration {
		lb = maxArchiveDuration
	}
	return lb
}

func mediaSequenceForTimeshift(firstStart time.Time, segmentDuration time.Duration) uint64 {
	if firstStart.IsZero() {
		return 0
	}
	step := int64(segmentDuration / time.Second)
	if step < 1 {
		step = 1
	}
	return uint64(firstStart.Unix() / step)
}

// timeshiftSegDuration returns the real segment length from neighbouring starts,
// clipped to delayedEdge for the trailing segment (Flussonic-style EXTINF).
func timeshiftSegDuration(
	segs []*IndexedSegment,
	i int,
	nominal time.Duration,
	delayedEdge time.Time,
) time.Duration {
	if i < 0 || i >= len(segs) {
		return nominal
	}
	seg := segs[i]
	var dur time.Duration
	if i+1 < len(segs) {
		dur = segs[i+1].Start.Sub(seg.Start)
	} else if !delayedEdge.IsZero() && delayedEdge.After(seg.Start) {
		dur = delayedEdge.Sub(seg.Start)
	} else if seg.fmp4.Duration > 0 {
		dur = seg.fmp4.Duration
	} else {
		dur = nominal
	}
	if dur <= 0 {
		dur = nominal
	}
	if nominal > 0 && dur > 2*nominal {
		dur = nominal
	}
	return dur
}

// trimTimeshiftSegs drops a trailing segment that is still too short at the delayed edge.
func trimTimeshiftSegs(segs []*IndexedSegment, nominal time.Duration, delayedEdge time.Time) []*IndexedSegment {
	if len(segs) == 0 {
		return segs
	}
	minDur := time.Second
	if nominal > 0 && nominal/2 > minDur {
		minDur = nominal / 2
	}
	for len(segs) > 0 {
		last := len(segs) - 1
		if timeshiftSegDuration(segs, last, nominal, delayedEdge) >= minDur {
			break
		}
		segs = segs[:last]
	}
	return segs
}

func generateM3U8MPEGTSIndexed(
	segs []*IndexedSegment,
	segmentDuration time.Duration,
	timeOffsetMinutes int,
	windowStart time.Time,
) string {
	items := make([]*recordstore.Segment, 0, len(segs))
	names := make([]string, 0, len(segs))
	for _, s := range segs {
		if s.Name() == "" {
			continue
		}
		names = append(names, s.Name())
		items = append(items, &recordstore.Segment{Start: s.Start})
	}
	return writeM3U8MPEGTS(items, names, segmentDuration, timeOffsetMinutes, windowStart)
}

// GenerateTimeshiftM3U8Indexed builds a sliding delayed-live playlist (no ENDLIST).
// delayedEdge is now-ago; trailing segment duration is clipped to it.
func GenerateTimeshiftM3U8Indexed(
	format conf.RecordFormat,
	segs []*IndexedSegment,
	segmentDuration time.Duration,
	chunkDuration time.Duration,
	timeOffsetMinutes int,
	delayedEdge time.Time,
) string {
	segs = trimTimeshiftSegs(segs, segmentDuration, delayedEdge)
	if format == conf.RecordFormatFMP4 {
		return generateM3U8FMP4Timeshift(segs, segmentDuration, chunkDuration, timeOffsetMinutes, delayedEdge)
	}
	return writeM3U8MPEGTSTimeshift(segs, segmentDuration, timeOffsetMinutes, delayedEdge)
}

// GenerateArchiveM3U8 builds a VOD playlist for the configured record format.
// fMP4 entries are inspected from disk; prefer GenerateArchiveM3U8Indexed for request serving.
func GenerateArchiveM3U8(
	format conf.RecordFormat,
	segments []*recordstore.Segment,
	segmentDuration time.Duration,
	chunkDuration time.Duration,
	timeOffsetMinutes int,
) string {
	if format == conf.RecordFormatFMP4 {
		return generateM3U8FMP4(segments, segmentDuration, chunkDuration, timeOffsetMinutes)
	}
	return generateM3U8MPEGTS(segments, segmentDuration, timeOffsetMinutes, time.Time{})
}

// GenerateArchiveM3U8Indexed builds a VOD playlist from the in-memory index (no disk I/O
// for mpegts; no disk I/O for fMP4 segments that were inspected at startup/complete).
// windowStart is the archive-{from}-* timestamp; when the first file starts earlier,
// EXT-X-START skips to that instant so the DVR player's PDT+currentTime clock
// matches the requested from (vanilla hls.js/VLC ignore this and play from 0).
func GenerateArchiveM3U8Indexed(
	format conf.RecordFormat,
	segs []*IndexedSegment,
	segmentDuration time.Duration,
	chunkDuration time.Duration,
	timeOffsetMinutes int,
	windowStart time.Time,
) string {
	if format == conf.RecordFormatFMP4 {
		return generateM3U8FMP4Indexed(segs, segmentDuration, chunkDuration, timeOffsetMinutes, windowStart)
	}
	return generateM3U8MPEGTSIndexed(segs, segmentDuration, timeOffsetMinutes, windowStart)
}

// GenerateM3U8 builds a VOD playlist with PROGRAM-DATE-TIME and DISCONTINUITY on gaps (mpegts).
func GenerateM3U8(
	segments []*recordstore.Segment,
	segmentDuration time.Duration,
	timeOffsetMinutes int,
) string {
	return generateM3U8MPEGTS(segments, segmentDuration, timeOffsetMinutes, time.Time{})
}

func generateM3U8MPEGTS(
	segments []*recordstore.Segment,
	segmentDuration time.Duration,
	timeOffsetMinutes int,
	windowStart time.Time,
) string {
	names := make([]string, len(segments))
	for i, seg := range segments {
		names[i] = filepath.Base(seg.Fpath)
	}
	return writeM3U8MPEGTS(segments, names, segmentDuration, timeOffsetMinutes, windowStart)
}

func writeM3U8MPEGTS(
	segments []*recordstore.Segment,
	names []string,
	segmentDuration time.Duration,
	timeOffsetMinutes int,
	windowStart time.Time,
) string {
	targetDur := int(segmentDuration.Seconds())
	if targetDur < 1 {
		targetDur = 1
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-VERSION:10\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDur)
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")

	if len(segments) == 0 {
		b.WriteString("#EXT-X-ENDLIST\n")
		return b.String()
	}

	writePlaylistStart(&b, segments[0].Start, windowStart)

	gapThreshold := playlistGapThreshold(segmentDuration)

	var lastEnd time.Time
	extinf := segmentDuration.Seconds()
	origin := segments[0].Start
	elapsed := time.Duration(0)

	for i, seg := range segments {
		if i > 0 {
			gap := seg.Start.Sub(lastEnd)
			if gap > gapThreshold {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
		}

		fmt.Fprintf(&b, "#EXTINF:%.1f,\n", extinf)
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", formatProgramDateTime(origin.Add(elapsed), timeOffsetMinutes))
		b.WriteString(names[i])
		b.WriteByte('\n')

		elapsed += segmentDuration
		lastEnd = seg.Start.Add(segmentDuration)
	}

	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// writeM3U8MPEGTSTimeshift emits a Flussonic-like sliding live playlist:
// short window, real EXTINF, single leading PDT, no ENDLIST.
func writeM3U8MPEGTSTimeshift(
	segs []*IndexedSegment,
	segmentDuration time.Duration,
	timeOffsetMinutes int,
	delayedEdge time.Time,
) string {
	type item struct {
		name  string
		start time.Time
		dur   time.Duration
	}
	items := make([]item, 0, len(segs))
	maxDur := time.Duration(0)
	for i, seg := range segs {
		if seg.Name() == "" {
			continue
		}
		dur := timeshiftSegDuration(segs, i, segmentDuration, delayedEdge)
		if dur <= 0 {
			continue
		}
		items = append(items, item{name: seg.Name(), start: seg.Start, dur: dur})
		if dur > maxDur {
			maxDur = dur
		}
	}

	targetDur := int(math.Ceil(maxDur.Seconds() - 1e-9))
	if targetDur < 1 {
		if segmentDuration > 0 {
			targetDur = int(math.Ceil(segmentDuration.Seconds() - 1e-9))
		}
		if targetDur < 1 {
			targetDur = 1
		}
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDur)
	b.WriteString("#EXT-X-VERSION:3\n")

	mediaSeq := uint64(0)
	if len(items) > 0 {
		mediaSeq = mediaSequenceForTimeshift(items[0].start, segmentDuration)
	}
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", mediaSeq)

	if len(items) == 0 {
		return b.String()
	}

	fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", formatProgramDateTimeUTC(items[0].start, timeOffsetMinutes))

	gapThreshold := playlistGapThreshold(segmentDuration)
	var lastEnd time.Time
	for i, it := range items {
		if i > 0 {
			gap := it.start.Sub(lastEnd)
			if gap > gapThreshold {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", it.dur.Seconds())
		b.WriteString(it.name)
		b.WriteByte('\n')
		lastEnd = it.start.Add(it.dur)
	}
	return b.String()
}

// formatProgramDateTimeUTC formats PDT like Flussonic (…Z when offset is 0).
func formatProgramDateTimeUTC(t time.Time, offsetMinutes int) string {
	if offsetMinutes == 0 {
		return t.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return formatProgramDateTime(t, offsetMinutes)
}

type fmp4PlaylistSeg struct {
	name      string
	fpath     string
	start     time.Time
	duration  time.Duration
	ptsEnd    time.Duration // last sample PTS+dur; set only on sliced HLS chunks
	moofCount uint32
	tracks    []*fmp4.InitTrack
	off       int64
	n         int
}

func durationToTdMs(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Round(time.Millisecond).Milliseconds()
}

func generateM3U8FMP4(
	segments []*recordstore.Segment,
	segmentDuration time.Duration,
	chunkDuration time.Duration,
	timeOffsetMinutes int,
) string {
	infos := make([]fmp4PlaylistSeg, 0, len(segments))
	for _, seg := range segments {
		meta, tracks, err := inspectFMP4Segment(seg.Fpath)
		if err != nil {
			continue
		}
		if meta.Duration <= 0 {
			continue
		}
		infos = append(infos, fmp4PlaylistSeg{
			name:      filepath.Base(seg.Fpath),
			fpath:     seg.Fpath,
			start:     seg.Start,
			duration:  meta.Duration,
			moofCount: meta.MoofCount,
			tracks:    tracks,
		})
	}
	infos = expandFMP4PlaylistSegs(infos, chunkDuration)
	return writeM3U8FMP4(infos, segmentDuration, timeOffsetMinutes, time.Time{})
}

func generateM3U8FMP4Indexed(
	segs []*IndexedSegment,
	segmentDuration time.Duration,
	chunkDuration time.Duration,
	timeOffsetMinutes int,
	windowStart time.Time,
) string {
	infos := make([]fmp4PlaylistSeg, 0, len(segs))
	now := time.Now()
	for i, seg := range segs {
		if seg.Name() == "" {
			continue
		}
		dur := segmentDurationForPlaylist(seg, segs, i, segmentDuration, now)
		if dur <= 0 {
			continue
		}
		infos = append(infos, fmp4PlaylistSeg{
			name:      seg.Name(),
			fpath:     seg.Fpath(),
			start:     seg.Start,
			duration:  dur,
			moofCount: seg.fmp4.MoofCount,
			tracks:    seg.tracks(),
		})
	}
	infos = expandFMP4PlaylistSegs(infos, chunkDuration)
	return writeM3U8FMP4(infos, segmentDuration, timeOffsetMinutes, windowStart)
}

func generateM3U8FMP4Timeshift(
	segs []*IndexedSegment,
	segmentDuration time.Duration,
	chunkDuration time.Duration,
	timeOffsetMinutes int,
	delayedEdge time.Time,
) string {
	infos := make([]fmp4PlaylistSeg, 0, len(segs))
	for i, seg := range segs {
		if seg.Name() == "" {
			continue
		}
		dur := timeshiftSegDuration(segs, i, segmentDuration, delayedEdge)
		if dur <= 0 {
			continue
		}
		infos = append(infos, fmp4PlaylistSeg{
			name:      seg.Name(),
			fpath:     seg.Fpath(),
			start:     seg.Start,
			duration:  dur,
			moofCount: seg.fmp4.MoofCount,
			tracks:    seg.tracks(),
		})
	}
	infos = expandFMP4PlaylistSegs(infos, chunkDuration)
	step := hlsPlaylistStep(segmentDuration, chunkDuration)
	windowStart := delayedEdge.Add(-timeshiftLookback(step))
	infos = filterTimeshiftFMP4Chunks(infos, windowStart, delayedEdge)
	return writeM3U8FMP4Timeshift(infos, segmentDuration, step, timeOffsetMinutes)
}

func expandFMP4PlaylistSegs(infos []fmp4PlaylistSeg, chunkDuration time.Duration) []fmp4PlaylistSeg {
	if chunkDuration <= 0 || len(infos) == 0 {
		return infos
	}
	out := make([]fmp4PlaylistSeg, 0, len(infos))
	for _, info := range infos {
		out = append(out, expandFMP4PlaylistSeg(info, chunkDuration)...)
	}
	return out
}

func expandFMP4PlaylistSeg(info fmp4PlaylistSeg, chunkDuration time.Duration) []fmp4PlaylistSeg {
	if !shouldSliceFMP4(info.duration, chunkDuration) || info.fpath == "" {
		return []fmp4PlaylistSeg{info}
	}
	parts, err := loadFMP4MediaParts(info.fpath)
	if err != nil || len(parts) == 0 {
		return []fmp4PlaylistSeg{info}
	}
	chunks := groupHLSChunks(parts, chunkDuration)
	if len(chunks) <= 1 {
		return []fmp4PlaylistSeg{info}
	}
	out := make([]fmp4PlaylistSeg, 0, len(chunks))
	elapsed := time.Duration(0)
	for _, ch := range chunks {
		dur := ch.Duration
		if dur <= 0 {
			elapsed += dur
			continue
		}
		item := info
		item.start = info.start.Add(elapsed)
		item.duration = dur
		item.ptsEnd = ch.PTSEnd
		item.moofCount = ch.MoofCount
		item.off = ch.Off
		item.n = ch.N
		out = append(out, item)
		elapsed += dur
	}
	if len(out) <= 1 {
		return []fmp4PlaylistSeg{info}
	}
	return out
}

func filterTimeshiftFMP4Chunks(infos []fmp4PlaylistSeg, windowStart, delayedEdge time.Time) []fmp4PlaylistSeg {
	if delayedEdge.IsZero() || len(infos) == 0 {
		return infos
	}
	out := make([]fmp4PlaylistSeg, 0, len(infos))
	for _, info := range infos {
		if !info.start.Before(delayedEdge) {
			continue
		}
		end := info.start.Add(info.duration)
		if !windowStart.IsZero() && !end.After(windowStart) {
			continue
		}
		if end.After(delayedEdge) {
			info.duration = delayedEdge.Sub(info.start)
			if info.duration <= 0 {
				continue
			}
		}
		out = append(out, info)
	}
	return out
}

func fmp4MediaURI(info fmp4PlaylistSeg, seq uint32, tdMs int64) string {
	if info.n > 0 {
		return fmt.Sprintf("%s?hls=media&off=%d&n=%d&sn=%d&td=%d", info.name, info.off, info.n, seq, tdMs)
	}
	return fmt.Sprintf("%s?hls=media&sn=%d&td=%d", info.name, seq, tdMs)
}

func segmentDurationForPlaylist(
	seg *IndexedSegment,
	segs []*IndexedSegment,
	i int,
	nominal time.Duration,
	now time.Time,
) time.Duration {
	if seg.fmp4.Duration > 0 {
		return seg.fmp4.Duration
	}
	if i+1 < len(segs) {
		if delta := segs[i+1].Start.Sub(seg.Start); delta > 0 {
			return delta
		}
	}
	if seg.fmp4.Ready {
		return 0
	}
	dur := now.Sub(seg.Start)
	if dur <= 0 {
		return 0
	}
	if nominal > 0 && dur > nominal {
		return nominal
	}
	return dur
}

func writePlaylistStart(b *strings.Builder, firstStart, windowStart time.Time) {
	if windowStart.IsZero() || firstStart.IsZero() || !windowStart.After(firstStart) {
		return
	}
	off := windowStart.Sub(firstStart).Seconds()
	if off < 0.05 {
		return
	}
	fmt.Fprintf(b, "#EXT-X-START:TIME-OFFSET=%.3f\n", off)
}

func writeM3U8FMP4(
	infos []fmp4PlaylistSeg,
	segmentDuration time.Duration,
	timeOffsetMinutes int,
	windowStart time.Time,
) string {
	maxDur := time.Duration(0)
	for i := range infos {
		if infos[i].duration > maxDur {
			maxDur = infos[i].duration
		}
	}
	if maxDur <= 0 {
		maxDur = segmentDuration
	}

	targetDur := int(math.Ceil(maxDur.Seconds() - 1e-9))
	if targetDur < 1 {
		targetDur = 1
	}

	var b strings.Builder
	if n := len(infos); n > 0 {
		b.Grow(160 + n*180)
	}
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDur)
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	if len(infos) == 0 {
		b.WriteString("#EXT-X-ENDLIST\n")
		return b.String()
	}

	writePlaylistStart(&b, infos[0].start, windowStart)

	softGap := playlistGapThreshold(segmentDuration)
	gapThreshold := softGap
	if maxDur+maxDur/2 > gapThreshold {
		gapThreshold = maxDur + maxDur/2
	}

	var (
		lastEnd    time.Time
		prevTracks []*fmp4.InitTrack
		prevName   string
		seq        uint32
		tdMs       int64
		fileTdMs   int64
		elapsed    time.Duration
	)
	origin := infos[0].start

	for i, info := range infos {
		needMap := i == 0
		discontinuity := false

		if i > 0 {
			gap := info.start.Sub(lastEnd)
			sameCodec := true
			if prevTracks != nil && info.tracks != nil {
				sameCodec = fmp4TracksCompatible(prevTracks, info.tracks)
			}
			if gap > gapThreshold || !sameCodec {
				discontinuity = true
				needMap = true
				seq = 0
				tdMs = 0
			}
		}

		if i == 0 || discontinuity || info.name != prevName {
			fileTdMs = tdMs
			if i > 0 && !discontinuity && info.name != prevName {
				needMap = false
			}
		}

		if discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if needMap {
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s?hls=init\"\n", info.name)
		}

		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", info.duration.Seconds())
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", formatProgramDateTime(origin.Add(elapsed), timeOffsetMinutes))
		b.WriteString(fmp4MediaURI(info, seq, fileTdMs))
		b.WriteByte('\n')

		seq += info.moofCount
		tdMs = nextFileTdMs(fileTdMs, tdMs, info)
		elapsed += info.duration
		lastEnd = info.start.Add(info.duration)
		prevTracks = info.tracks
		prevName = info.name
	}

	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func writeM3U8FMP4Timeshift(
	infos []fmp4PlaylistSeg,
	segmentDuration time.Duration,
	seqStep time.Duration,
	timeOffsetMinutes int,
) string {
	maxDur := time.Duration(0)
	for i := range infos {
		if infos[i].duration > maxDur {
			maxDur = infos[i].duration
		}
	}
	if maxDur <= 0 {
		maxDur = segmentDuration
	}

	targetDur := int(math.Ceil(maxDur.Seconds() - 1e-9))
	if targetDur < 1 {
		targetDur = 1
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDur)
	b.WriteString("#EXT-X-VERSION:7\n")

	mediaSeq := uint64(0)
	if len(infos) > 0 {
		step := seqStep
		if step <= 0 {
			step = segmentDuration
		}
		mediaSeq = mediaSequenceForTimeshift(infos[0].start, step)
	}
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", mediaSeq)
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	if len(infos) == 0 {
		return b.String()
	}

	fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", formatProgramDateTimeUTC(infos[0].start, timeOffsetMinutes))

	softGap := playlistGapThreshold(segmentDuration)
	gapThreshold := softGap
	if maxDur+maxDur/2 > gapThreshold {
		gapThreshold = maxDur + maxDur/2
	}

	var (
		lastEnd    time.Time
		prevTracks []*fmp4.InitTrack
		prevName   string
		seq        uint32
		tdMs       int64
		fileTdMs   int64
	)

	for i, info := range infos {
		needMap := i == 0
		discontinuity := false

		if i > 0 {
			gap := info.start.Sub(lastEnd)
			sameCodec := true
			if prevTracks != nil && info.tracks != nil {
				sameCodec = fmp4TracksCompatible(prevTracks, info.tracks)
			}
			if gap > gapThreshold || !sameCodec {
				discontinuity = true
				needMap = true
				seq = 0
				tdMs = 0
			}
		}

		if i == 0 || discontinuity || info.name != prevName {
			fileTdMs = tdMs
		}

		if discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if needMap {
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s?hls=init\"\n", info.name)
		}

		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", info.duration.Seconds())
		b.WriteString(fmp4MediaURI(info, seq, fileTdMs))
		b.WriteByte('\n')

		seq += info.moofCount
		tdMs = nextFileTdMs(fileTdMs, tdMs, info)
		lastEnd = info.start.Add(info.duration)
		prevTracks = info.tracks
		prevName = info.name
	}
	return b.String()
}

// nextFileTdMs is the offset of the next playlist item.
// Unsliced 5s files keep the old EXTINF sum. Sliced long files use the last
// sample PTS+duration of the current file (tfdt), not the sum of chunk EXTINF.
func nextFileTdMs(fileTdMs, tdMs int64, info fmp4PlaylistSeg) int64 {
	if info.n > 0 && info.ptsEnd > 0 {
		return fileTdMs + durationToTdMs(info.ptsEnd)
	}
	return tdMs + info.duration.Milliseconds()
}

func mergeURIQuery(uri, extraQuery string) string {
	if extraQuery == "" {
		return uri
	}
	if strings.Contains(uri, "?") {
		return uri + "&" + extraQuery
	}
	return uri + "?" + extraQuery
}

// appendQueryToPlaylistURIs copies extraQuery onto media URIs and EXT-X-MAP
// so HLS players send the same auth token / session on segment requests.
func appendQueryToPlaylistURIs(body, extraQuery string) string {
	if extraQuery == "" {
		return body
	}

	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MAP:") {
			const prefix = `URI="`
			start := strings.Index(line, prefix)
			if start < 0 {
				continue
			}
			start += len(prefix)
			end := strings.Index(line[start:], `"`)
			if end < 0 {
				continue
			}
			uri := line[start : start+end]
			lines[i] = line[:start] + mergeURIQuery(uri, extraQuery) + line[start+end:]
			continue
		}
		if line[0] == '#' {
			continue
		}
		lines[i] = mergeURIQuery(line, extraQuery)
	}
	return strings.Join(lines, "\n")
}

func formatProgramDateTime(t time.Time, offsetMinutes int) string {
	offset := time.FixedZone("", offsetMinutes*60)
	local := t.In(offset)
	sign := "+"
	absMin := offsetMinutes
	if absMin < 0 {
		sign = "-"
		absMin = -absMin
	}
	return fmt.Sprintf("%s%s%02d:%02d",
		local.Format("2006-01-02T15:04:05.000"),
		sign,
		absMin/60,
		absMin%60,
	)
}
