package compatapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/stretchr/testify/require"
)

func TestBuildRanges(t *testing.T) {
	segDur := 10 * time.Second
	base := time.Unix(1000, 0).UTC()

	segments := []*recordstore.Segment{
		{Start: base},
		{Start: base.Add(10 * time.Second)},
		{Start: base.Add(20 * time.Second)},
		// gap
		{Start: base.Add(60 * time.Second)},
		{Start: base.Add(70 * time.Second)},
	}

	ranges := BuildRanges(segments, segDur)
	require.Equal(t, []RecordingRange{
		{From: 1000, Duration: 30},
		{From: 1060, Duration: 20},
	}, ranges)
}

func TestBuildRangesDoesNotFillRealGaps(t *testing.T) {
	// 1h nominal (MediaMTX default) used to merge holes up to 2h — that makes the
	// Flussonic DVR player jump across missing video.
	segDur := time.Hour
	base := time.Unix(1_000_000, 0).UTC()
	segments := []*recordstore.Segment{
		{Start: base},
		{Start: base.Add(time.Hour)},
		// 10-minute hole
		{Start: base.Add(2*time.Hour + 10*time.Minute)},
	}
	ranges := BuildRanges(segments, segDur)
	require.Equal(t, []RecordingRange{
		{From: 1_000_000, Duration: 7200},
		{From: 1_000_000 + 2*3600 + 600, Duration: 3600},
	}, ranges)
}

func TestBuildRangesClampsFutureTail(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	segs := []timedSeg{
		{Start: time.Unix(1990, 0).UTC(), Duration: time.Hour},
	}
	ranges := buildRanges(segs, time.Hour, now)
	require.Equal(t, []RecordingRange{
		{From: 1990, Duration: 10},
	}, ranges)
}

func TestParseUnixQuery(t *testing.T) {
	v, ok := parseUnixQuery("1786647210000")
	require.True(t, ok)
	require.Equal(t, int64(1786647210), v)

	v, ok = parseUnixQuery("1786647210")
	require.True(t, ok)
	require.Equal(t, int64(1786647210), v)

	_, ok = parseUnixQuery("")
	require.False(t, ok)
}

func TestBuildRangesJSONFilterLimitResolution(t *testing.T) {
	ranges := []RecordingRange{
		{From: 1000, Duration: 20},
		{From: 1030, Duration: 10},
		{From: 2000, Duration: 50},
	}

	q := parseRangesQuery(func(k string) string {
		switch k {
		case "closed_at_gte":
			return "1025"
		case "opened_at_lte":
			return "1040"
		case "limit":
			return "1000"
		case "resolution":
			return "0"
		default:
			return ""
		}
	})
	out := buildRangesJSON(ranges, q)
	require.Equal(t, 1, out.EstimatedCount)
	require.Equal(t, []DVRRange{
		{Duration: 10, From: 1030, OpenedAt: 1030, ClosedAt: 1040},
	}, out.Ranges)

	q = parseRangesQuery(func(k string) string {
		switch k {
		case "closed_at_gte":
			return "1010"
		case "opened_at_lte":
			return "2000"
		case "limit":
			return "1"
		default:
			return ""
		}
	})
	out = buildRangesJSON(ranges, q)
	require.Equal(t, 3, out.EstimatedCount)
	require.Len(t, out.Ranges, 1)
	require.Equal(t, int64(1000), out.Ranges[0].From)

	q = parseRangesQuery(func(k string) string {
		if k == "resolution" {
			return "15"
		}
		return ""
	})
	out = buildRangesJSON(ranges[:2], q)
	require.Equal(t, 1, out.EstimatedCount)
	require.Equal(t, DVRRange{Duration: 40, From: 1000, OpenedAt: 1000, ClosedAt: 1040}, out.Ranges[0])
}

func TestBuildRangesJSONMillisecondsFilter(t *testing.T) {
	ranges := []RecordingRange{
		{From: 1786646565, Duration: 1211},
	}
	q := parseRangesQuery(func(k string) string {
		switch k {
		case "closed_at_gte":
			return "1786647210000"
		case "opened_at_lte":
			return "1786648110000"
		default:
			return ""
		}
	})
	out := buildRangesJSON(ranges, q)
	require.Equal(t, 1, out.EstimatedCount)
	require.Equal(t, DVRRange{
		Duration: 1211,
		From:     1786646565,
		OpenedAt: 1786646565,
		ClosedAt: 1786647776,
	}, out.Ranges[0])
}

func TestGenerateM3U8(t *testing.T) {
	base := time.Unix(1758456000, 0).UTC()
	segments := []*recordstore.Segment{
		{Fpath: filepath.Join("cam", "a.ts"), Start: base},
		{Fpath: filepath.Join("cam", "b.ts"), Start: base.Add(10 * time.Second)},
		{Fpath: filepath.Join("cam", "c.ts"), Start: base.Add(40 * time.Second)}, // discontinuity
	}

	body := GenerateM3U8(segments, 10*time.Second, 180)
	require.Contains(t, body, "#EXT-X-PLAYLIST-TYPE:VOD")
	require.Contains(t, body, "#EXTINF:10.0,")
	require.Contains(t, body, "a.ts")
	require.Contains(t, body, "b.ts")
	require.Contains(t, body, "c.ts")
	require.Contains(t, body, "#EXT-X-DISCONTINUITY")
	require.Contains(t, body, "#EXT-X-PROGRAM-DATE-TIME:2025-09-21T15:00:00.000+03:00")
	require.Contains(t, body, "#EXT-X-ENDLIST")
}

func TestGenerateArchiveM3U8FMP4(t *testing.T) {
	dir := t.TempDir()

	writeSeg := func(name string, moofs int) string {
		t.Helper()
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		require.NoError(t, err)
		defer f.Close()

		init := fmp4.Init{
			Tracks: []*fmp4.InitTrack{
				{
					ID:        1,
					TimeScale: 1000,
					Codec: &mcodecs.H264{
						SPS: []byte{
							0x67, 0x64, 0x00, 0x1e, 0xac, 0xd9, 0x40, 0xa0,
							0x3d, 0xa1, 0x00, 0x00, 0x03, 0x00, 0x01, 0x00,
							0x00, 0x03, 0x00, 0x32, 0x8f, 0x18, 0x32, 0x48,
						},
						PPS: []byte{0x68, 0xee, 0x3c, 0xb0},
					},
				},
			},
		}
		require.NoError(t, init.Marshal(f))
		for i := 0; i < moofs; i++ {
			part := fmp4.Part{
				SequenceNumber: uint32(i),
				Tracks: []*fmp4.PartTrack{
					{
						ID:       1,
						BaseTime: uint64(i * 1000),
						Samples: []*fmp4.Sample{
							{Duration: 1000, Payload: []byte{0, 0, 0, 1, 9, 0xf0}},
						},
					},
				},
			}
			require.NoError(t, part.Marshal(f))
		}
		return path
	}

	a := writeSeg("a.mp4", 2)
	b := writeSeg("b.mp4", 3)

	base := time.Unix(1000, 0).UTC()
	body := GenerateArchiveM3U8(conf.RecordFormatFMP4, []*recordstore.Segment{
		{Fpath: a, Start: base},
		{Fpath: b, Start: base.Add(12 * time.Second)},
	}, 10*time.Second, 0)

	require.Equal(t, 1, strings.Count(body, "#EXT-X-MAP:"))
	require.Contains(t, body, `#EXT-X-MAP:URI="a.mp4?hls=init"`)
	require.NotContains(t, body, `#EXT-X-MAP:URI="b.mp4?hls=init"`)
	require.NotContains(t, body, "#EXT-X-DISCONTINUITY")
	require.NotContains(t, body, "#EXT-X-BYTERANGE")
	require.Contains(t, body, "a.mp4?hls=media&sn=0&td=0")
	require.Contains(t, body, "b.mp4?hls=media&sn=2&td=2000")
	require.Contains(t, body, "#EXTINF:2.000,")
	require.Contains(t, body, "#EXTINF:3.000,")
	require.Contains(t, body, "#EXT-X-TARGETDURATION:3")
	require.NotContains(t, body, "#EXTINF:10.000,")
	require.NotContains(t, body, "#EXTINF:12.000,")
}

func TestGenerateArchiveM3U8FMP4DiscontinuityOnGap(t *testing.T) {
	dir := t.TempDir()

	writeSeg := func(name string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		require.NoError(t, err)
		defer f.Close()
		init := fmp4.Init{
			Tracks: []*fmp4.InitTrack{
				{
					ID:        1,
					TimeScale: 90000,
					Codec: &mcodecs.H264{
						SPS: []byte{
							0x67, 0x64, 0x00, 0x1e, 0xac, 0xd9, 0x40, 0xa0,
							0x3d, 0xa1, 0x00, 0x00, 0x03, 0x00, 0x01, 0x00,
							0x00, 0x03, 0x00, 0x32, 0x8f, 0x18, 0x32, 0x48,
						},
						PPS: []byte{0x68, 0xee, 0x3c, 0xb0},
					},
				},
			},
		}
		require.NoError(t, init.Marshal(f))
		part := fmp4.Part{
			SequenceNumber: 0,
			Tracks: []*fmp4.PartTrack{
				{
					ID: 1,
					Samples: []*fmp4.Sample{
						{Duration: 90000, Payload: []byte{0, 0, 0, 1, 9, 0xf0}},
					},
				},
			},
		}
		require.NoError(t, part.Marshal(f))
		return path
	}

	a := writeSeg("a.mp4")
	b := writeSeg("b.mp4")
	base := time.Unix(1000, 0).UTC()
	body := GenerateArchiveM3U8(conf.RecordFormatFMP4, []*recordstore.Segment{
		{Fpath: a, Start: base},
		{Fpath: b, Start: base.Add(60 * time.Second)},
	}, 10*time.Second, 0)
	require.Contains(t, body, "#EXT-X-DISCONTINUITY")
	require.Equal(t, 2, strings.Count(body, "#EXT-X-MAP:"))
	require.Contains(t, body, `#EXT-X-MAP:URI="a.mp4?hls=init"`)
	require.Contains(t, body, `#EXT-X-MAP:URI="b.mp4?hls=init"`)
	require.Contains(t, body, "a.mp4?hls=media&sn=0&td=0")
	require.Contains(t, body, "b.mp4?hls=media&sn=0&td=0")
	require.Contains(t, body, "#EXTINF:1.000,")
	require.NotContains(t, body, "#EXT-X-BYTERANGE")
}

func TestFormatProgramDateTimeUTC(t *testing.T) {
	ts := time.Unix(0, 0).UTC()
	require.Equal(t, "1970-01-01T00:00:00.000+00:00", formatProgramDateTime(ts, 0))
}

func TestGenerateArchiveM3U8IndexedMPEGTS(t *testing.T) {
	base := time.Unix(1758456000, 0).UTC()
	body := GenerateArchiveM3U8Indexed(conf.RecordFormatMPEGTS, []*IndexedSegment{
		{Rel: "a.ts", Start: base},
		{Rel: "b.ts", Start: base.Add(10 * time.Second)},
		{Rel: "c.ts", Start: base.Add(40 * time.Second)},
		{Rel: "", Start: base.Add(50 * time.Second)},
	}, 10*time.Second, 180, time.Time{})
	require.Contains(t, body, "#EXTINF:10.0,")
	require.Contains(t, body, "a.ts")
	require.NotContains(t, body, "other.ts")
	require.NotContains(t, body, "skip.ts")
	require.Contains(t, body, "#EXT-X-DISCONTINUITY")
	require.Contains(t, body, "#EXT-X-ENDLIST")
}

func TestGenerateArchiveM3U8IndexedFMP4NoDisk(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	body := GenerateArchiveM3U8Indexed(conf.RecordFormatFMP4, []*IndexedSegment{
		{
			Rel:   "from-memory.mp4",
			Start: base,
			fmp4:  fmp4SegMeta{Duration: 10 * time.Second, MoofCount: 2, Ready: true},
		},
		{
			Rel:   "b.mp4",
			Start: base.Add(12 * time.Second),
			fmp4:  fmp4SegMeta{Duration: 10 * time.Second, MoofCount: 3, Ready: true},
		},
		{
			Rel:   "",
			Start: base.Add(22 * time.Second),
			fmp4:  fmp4SegMeta{Duration: 10 * time.Second, MoofCount: 1, Ready: true},
		},
	}, 10*time.Second, 0, time.Time{})

	require.Equal(t, 1, strings.Count(body, "#EXT-X-MAP:"))
	require.Contains(t, body, `#EXT-X-MAP:URI="from-memory.mp4?hls=init"`)
	require.Contains(t, body, "from-memory.mp4?hls=media&sn=0&td=0")
	require.Contains(t, body, "b.mp4?hls=media&sn=2&td=10000")
	require.NotContains(t, body, "other.mp4")
	require.NotContains(t, body, "skip.mp4")
	require.NotContains(t, body, "#EXT-X-DISCONTINUITY")
	require.NotContains(t, body, "#EXT-X-START")
	// PDT follows playlist time, not the later file's Start (12s).
	require.Contains(t, body, "#EXT-X-PROGRAM-DATE-TIME:1970-01-01T00:16:40.000+00:00")
	require.Contains(t, body, "#EXT-X-PROGRAM-DATE-TIME:1970-01-01T00:16:50.000+00:00")
}

func TestGenerateArchiveM3U8IndexedFMP4UsesExactDuration(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	body := GenerateArchiveM3U8Indexed(conf.RecordFormatFMP4, []*IndexedSegment{
		{
			Start: base,
			Rel:   "a.mp4",
			fmp4:  fmp4SegMeta{Duration: 4010 * time.Millisecond, MoofCount: 4, Ready: true},
		},
		{
			Start: base.Add(4010 * time.Millisecond),
			Rel:   "b.mp4",
			fmp4:  fmp4SegMeta{Duration: 3990 * time.Millisecond, MoofCount: 4, Ready: true},
		},
	}, 10*time.Second, 0, time.Time{})

	require.Contains(t, body, "#EXTINF:4.010,")
	require.Contains(t, body, "#EXTINF:3.990,")
	require.Contains(t, body, "#EXT-X-TARGETDURATION:5")
	require.NotContains(t, body, "#EXTINF:10.000,")
	require.Contains(t, body, "b.mp4?hls=media&sn=4&td=4010")
}

func TestGenerateArchiveM3U8IndexedStartOffsetAndMonotonicPDT(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	body := GenerateArchiveM3U8Indexed(conf.RecordFormatFMP4, []*IndexedSegment{
		{
			Rel:   "a.mp4",
			Start: base,
			fmp4:  fmp4SegMeta{Duration: 10 * time.Second, MoofCount: 2, Ready: true},
		},
		{
			Rel:   "b.mp4",
			Start: base.Add(8 * time.Second), // overlap: Start goes backwards vs EXTINF
			fmp4:  fmp4SegMeta{Duration: 10 * time.Second, MoofCount: 2, Ready: true},
		},
	}, 10*time.Second, 0, base.Add(4*time.Second))

	require.Contains(t, body, "#EXT-X-START:TIME-OFFSET=4.000")
	require.Contains(t, body, "#EXT-X-PROGRAM-DATE-TIME:1970-01-01T00:16:40.000+00:00")
	require.Contains(t, body, "#EXT-X-PROGRAM-DATE-TIME:1970-01-01T00:16:50.000+00:00")
	require.NotContains(t, body, "#EXT-X-PROGRAM-DATE-TIME:1970-01-01T00:16:48.000+00:00")
	require.NotContains(t, body, "#EXT-X-DISCONTINUITY")
	require.Contains(t, body, "a.mp4?hls=media&sn=0&td=0")
	require.Contains(t, body, "b.mp4?hls=media&sn=2&td=10000")
}

func TestGenerateArchiveM3U8IndexedMatchesDisk(t *testing.T) {
	dir := t.TempDir()
	writeSeg := func(name string, moofs int) string {
		t.Helper()
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		require.NoError(t, err)
		defer f.Close()
		init := fmp4.Init{
			Tracks: []*fmp4.InitTrack{
				{
					ID:        1,
					TimeScale: 1000,
					Codec: &mcodecs.H264{
						SPS: []byte{
							0x67, 0x64, 0x00, 0x1e, 0xac, 0xd9, 0x40, 0xa0,
							0x3d, 0xa1, 0x00, 0x00, 0x03, 0x00, 0x01, 0x00,
							0x00, 0x03, 0x00, 0x32, 0x8f, 0x18, 0x32, 0x48,
						},
						PPS: []byte{0x68, 0xee, 0x3c, 0xb0},
					},
				},
			},
		}
		require.NoError(t, init.Marshal(f))
		for i := 0; i < moofs; i++ {
			part := fmp4.Part{
				SequenceNumber: uint32(i),
				Tracks: []*fmp4.PartTrack{{
					ID:       1,
					BaseTime: uint64(i * 1000),
					Samples:  []*fmp4.Sample{{Duration: 1000, Payload: []byte{0, 0, 0, 1, 9, 0xf0}}},
				}},
			}
			require.NoError(t, part.Marshal(f))
		}
		return path
	}

	a := writeSeg("a.mp4", 2)
	b := writeSeg("b.mp4", 3)
	base := time.Unix(1000, 0).UTC()

	fromDisk := GenerateArchiveM3U8(conf.RecordFormatFMP4, []*recordstore.Segment{
		{Fpath: a, Start: base},
		{Fpath: b, Start: base.Add(12 * time.Second)},
	}, 10*time.Second, 0)

	idx := NewIndex()
	idx.Add("cam1", a, base)
	idx.Add("cam1", b, base.Add(12*time.Second))
	metaA, tracksA, err := inspectFMP4Segment(a)
	require.NoError(t, err)
	metaB, tracksB, err := inspectFMP4Segment(b)
	require.NoError(t, err)
	idx.SetFMP4Meta("cam1", a, metaA, tracksA)
	idx.SetFMP4Meta("cam1", b, metaB, tracksB)

	fromIndex := GenerateArchiveM3U8Indexed(
		conf.RecordFormatFMP4,
		idx.SegmentsInWindow("cam1", base, time.Minute),
		10*time.Second,
		0,
		time.Time{},
	)
	require.Equal(t, fromDisk, fromIndex)
}

func TestAppendQueryToPlaylistURIs(t *testing.T) {
	body := strings.Join([]string{
		"#EXTM3U",
		`#EXT-X-MAP:URI="a.mp4?hls=init"`,
		"#EXTINF:2.000,",
		"a.mp4?hls=media&sn=0&td=0",
		"b.ts",
		"#EXT-X-ENDLIST",
		"",
	}, "\n")

	got := appendQueryToPlaylistURIs(body, "token=secret")
	require.Contains(t, got, `#EXT-X-MAP:URI="a.mp4?hls=init&token=secret"`)
	require.Contains(t, got, "a.mp4?hls=media&sn=0&td=0&token=secret")
	require.Contains(t, got, "b.ts?token=secret")
	require.NotContains(t, got, "#EXTINF:2.000,?token=")
	require.Equal(t, body, appendQueryToPlaylistURIs(body, ""))
}
