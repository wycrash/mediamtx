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

// GenerateArchiveM3U8 builds a VOD playlist for the configured record format.
// fMP4 entries are inspected from disk; prefer GenerateArchiveM3U8Indexed for request serving.
func GenerateArchiveM3U8(
	format conf.RecordFormat,
	segments []*recordstore.Segment,
	segmentDuration time.Duration,
	timeOffsetMinutes int,
) string {
	if format == conf.RecordFormatFMP4 {
		return generateM3U8FMP4(segments, segmentDuration, timeOffsetMinutes)
	}
	return generateM3U8MPEGTS(segments, segmentDuration, timeOffsetMinutes, time.Time{})
}

// GenerateArchiveM3U8Indexed builds a VOD playlist from the in-memory index (no disk I/O
// for mpegts; no disk I/O for fMP4 segments that were inspected at startup/complete).
// windowStart is the archive-{from}-* timestamp; when the first file starts earlier,
// EXT-X-START skips to that instant so Flussonic DVR player's PDT+currentTime clock
// matches the requested from (vanilla hls.js/VLC ignore this and play from 0).
func GenerateArchiveM3U8Indexed(
	format conf.RecordFormat,
	segs []*IndexedSegment,
	segmentDuration time.Duration,
	timeOffsetMinutes int,
	windowStart time.Time,
) string {
	if format == conf.RecordFormatFMP4 {
		return generateM3U8FMP4Indexed(segs, segmentDuration, timeOffsetMinutes, windowStart)
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

type fmp4PlaylistSeg struct {
	name      string
	start     time.Time
	duration  time.Duration
	moofCount uint32
	tracks    []*fmp4.InitTrack
}

func generateM3U8FMP4(
	segments []*recordstore.Segment,
	segmentDuration time.Duration,
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
			start:     seg.Start,
			duration:  meta.Duration,
			moofCount: meta.MoofCount,
			tracks:    tracks,
		})
	}
	return writeM3U8FMP4(infos, segmentDuration, timeOffsetMinutes, time.Time{})
}

func generateM3U8FMP4Indexed(
	segs []*IndexedSegment,
	segmentDuration time.Duration,
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
			start:     seg.Start,
			duration:  dur,
			moofCount: seg.fmp4.MoofCount,
			tracks:    seg.tracks(),
		})
	}
	return writeM3U8FMP4(infos, segmentDuration, timeOffsetMinutes, windowStart)
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
		seq        uint32
		tdMs       int64
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

		if discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if needMap {
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s?hls=init\"\n", info.name)
		}

		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", info.duration.Seconds())
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", formatProgramDateTime(origin.Add(elapsed), timeOffsetMinutes))
		fmt.Fprintf(&b, "%s?hls=media&sn=%d&td=%d\n", info.name, seq, tdMs)

		seq += info.moofCount
		tdMs += info.duration.Milliseconds()
		elapsed += info.duration
		lastEnd = info.start.Add(info.duration)
		prevTracks = info.tracks
	}

	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
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
