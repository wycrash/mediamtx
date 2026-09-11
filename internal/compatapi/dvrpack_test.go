package compatapi

import (
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

const testPackHash = uint64(0xA1B2C3D4E5F60718)

func testPackDay(t *testing.T) (time.Time, int64) {
	t.Helper()
	day := time.Date(2020, 1, 2, 0, 0, 0, 0, time.Local)
	return day, day.Unix()
}

func testPackTracks() []*fmp4.InitTrack {
	return []*fmp4.InitTrack{{
		ID:        1,
		TimeScale: 90000,
		Codec: &mcodecs.H264{
			SPS: test.FormatH264.SPS,
			PPS: test.FormatH264.PPS,
		},
	}}
}

// A short segment carries no chunks, a long one carries the ranges the media
// handler serves. Both must survive a round trip with microsecond starts:
// the record template uses %f, so losing sub-millisecond precision would make
// the file name unreconstructable.
func TestDvrPackRoundTrip(t *testing.T) {
	day, dayUnix := testPackDay(t)
	short := day.Add(3*time.Hour + 4*time.Minute + 5*time.Second + 123456*time.Microsecond)
	long := short.Add(5 * time.Second)

	src := &dvrPack{hash: testPackHash, dayUnix: dayUnix, codecs: [][]*fmp4.InitTrack{testPackTracks()}}
	src.appendSeg(dvrPackSeg{
		Start: short, Duration: 5 * time.Second, PTSEnd: 5 * time.Second,
		Moof: 5, CodecID: 1, Ready: true,
	})
	src.appendSeg(dvrPackSeg{
		Start: long, Duration: time.Minute, PTSEnd: 60080 * time.Millisecond,
		Moof: 60, CodecID: 1, Ready: true,
		Name: "odd-name.mp4",
		Chunks: []dvrPackChunk{
			{Off: 2048, Duration: 6 * time.Second, PTSEnd: 6 * time.Second, Moof: 6},
			{Off: 400_000, Duration: 5 * time.Second, PTSEnd: 11 * time.Second, Moof: 5},
			{Off: 780_000, Duration: 49 * time.Second, PTSEnd: 60080 * time.Millisecond, Moof: 49},
		},
	})
	src.ranges = dvrPackRanges(src, 2*time.Second)
	require.Len(t, src.ranges, 1)

	raw, err := encodeDvrPack(src)
	require.NoError(t, err)

	got, err := decodeDvrPack(raw, testPackHash)
	require.NoError(t, err)
	require.Equal(t, 2, got.segCount())
	require.Equal(t, 3, got.chunkCount())
	require.Equal(t, src.ranges, got.ranges)
	require.Len(t, got.codecs, 1)
	require.Equal(t, uint32(90000), got.codecs[0][0].TimeScale)

	first := got.seg(0)
	require.True(t, first.Start.Equal(short), "want %s got %s", short, first.Start)
	require.Equal(t, short.Nanosecond()/1000, first.Start.Nanosecond()/1000)
	require.Equal(t, 5*time.Second, first.Duration)
	require.True(t, first.Ready)
	require.Empty(t, first.Chunks)
	require.Empty(t, first.Name)

	second := got.seg(1)
	require.True(t, second.Start.Equal(long))
	require.Equal(t, "odd-name.mp4", second.Name)
	require.Equal(t, uint16(60), second.Moof)
	require.Equal(t, 60080*time.Millisecond, second.PTSEnd)
	require.Equal(t, src.seg(1).Chunks, second.Chunks)
}

// The whole memory and disk budget rests on these two numbers: 30 days of a
// camera fit in RAM only while a segment costs 20 bytes and a chunk 16.
func TestDvrPackRecordSizes(t *testing.T) {
	_, dayUnix := testPackDay(t)
	base := time.Unix(dayUnix, 0)

	empty := &dvrPack{hash: testPackHash, dayUnix: dayUnix}
	rawEmpty, err := encodeDvrPack(empty)
	require.NoError(t, err)
	require.Len(t, rawEmpty, dvrPackHeaderSize+4)

	withSeg := &dvrPack{hash: testPackHash, dayUnix: dayUnix}
	withSeg.appendSeg(dvrPackSeg{Start: base, Duration: 5 * time.Second, Ready: true})
	rawSeg, err := encodeDvrPack(withSeg)
	require.NoError(t, err)
	require.Equal(t, dvrPackSegSize, len(rawSeg)-len(rawEmpty))

	withChunk := &dvrPack{hash: testPackHash, dayUnix: dayUnix}
	withChunk.appendSeg(dvrPackSeg{
		Start: base, Duration: 5 * time.Second, Ready: true,
		Chunks: []dvrPackChunk{{Off: 1, Duration: 5 * time.Second}},
	})
	rawChunk, err := encodeDvrPack(withChunk)
	require.NoError(t, err)
	require.Equal(t, dvrPackChunkSize, len(rawChunk)-len(rawSeg))
}

func TestDvrPackRejectsForeignHashAndCorruption(t *testing.T) {
	_, dayUnix := testPackDay(t)
	src := &dvrPack{hash: testPackHash, dayUnix: dayUnix}
	src.appendSeg(dvrPackSeg{Start: time.Unix(dayUnix, 0), Duration: 5 * time.Second, Ready: true})
	raw, err := encodeDvrPack(src)
	require.NoError(t, err)

	_, err = decodeDvrPack(raw, testPackHash+1)
	require.ErrorIs(t, err, errDvrPackHash)

	bad := append([]byte(nil), raw...)
	bad[0] = 'X'
	_, err = decodeDvrPack(bad, testPackHash)
	require.ErrorIs(t, err, errDvrPackMagic)

	bad = append([]byte(nil), raw...)
	bad[dvrPackHeaderSize+4]++
	_, err = decodeDvrPack(bad, testPackHash)
	require.ErrorIs(t, err, errDvrPackCRC)

	_, err = decodeDvrPack(raw[:dvrPackHeaderSize-1], testPackHash)
	require.ErrorIs(t, err, errDvrPackTrunc)
}

// Tier 0 must answer ranges without touching the record arrays, so the
// summary has to decode from the head of the file alone.
func TestDvrPackSummaryReadsHeadOnly(t *testing.T) {
	day, dayUnix := testPackDay(t)
	src := &dvrPack{hash: testPackHash, dayUnix: dayUnix}
	src.appendSeg(dvrPackSeg{Start: day.Add(time.Hour), Duration: 5 * time.Second, Ready: true})
	src.appendSeg(dvrPackSeg{Start: day.Add(2 * time.Hour), Duration: 5 * time.Second, Ready: true})
	src.ranges = dvrPackRanges(src, 2*time.Second)
	require.Len(t, src.ranges, 2)

	raw, err := encodeDvrPack(src)
	require.NoError(t, err)

	head := raw[:dvrPackHeaderSize+len(src.ranges)*dvrPackRangeSize]
	sum, err := decodeDvrPackSummary(head, testPackHash)
	require.NoError(t, err)
	require.Equal(t, dayUnix, sum.DayUnix)
	require.Equal(t, uint32(2), sum.NSeg)
	require.Equal(t, day.Add(time.Hour).UnixMicro(), sum.FirstStartUs)
	require.Equal(t, day.Add(2*time.Hour+5*time.Second).UnixMicro(), sum.LastEndUs)
	require.Equal(t, src.ranges, sum.Ranges)
}

func TestDvrPackFindSeg(t *testing.T) {
	day, dayUnix := testPackDay(t)
	src := &dvrPack{hash: testPackHash, dayUnix: dayUnix}
	for i := 0; i < 64; i++ {
		src.appendSeg(dvrPackSeg{
			Start:    day.Add(time.Duration(i) * 5 * time.Second),
			Duration: 5 * time.Second,
			Ready:    true,
		})
	}
	require.Equal(t, 0, src.findSeg(day))
	require.Equal(t, 17, src.findSeg(day.Add(85*time.Second)))
	require.Equal(t, -1, src.findSeg(day.Add(86*time.Second)))
	require.Equal(t, -1, src.findSeg(day.Add(-time.Second)))
}

func TestDvrWalRoundTripWithDeleteAndCodec(t *testing.T) {
	day, dayUnix := testPackDay(t)

	raw := encodeDvrWalHeader(testPackHash, dayUnix)
	codec, err := encodeDvrWalCodec(1, testPackTracks())
	require.NoError(t, err)
	raw = append(raw, codec...)

	for i := 0; i < 4; i++ {
		raw = append(raw, encodeDvrWalSeg(dvrPackSeg{
			Start:    day.Add(time.Duration(i) * time.Minute),
			Duration: time.Minute,
			PTSEnd:   time.Minute,
			Moof:     60,
			CodecID:  1,
			Ready:    true,
			Chunks: []dvrPackChunk{
				{Off: 2048, Duration: 30 * time.Second, PTSEnd: 30 * time.Second, Moof: 30},
				{Off: 500_000, Duration: 30 * time.Second, PTSEnd: time.Minute, Moof: 30},
			},
		}, dayUnix)...)
	}
	raw = append(raw, encodeDvrWalDelete(day.Add(time.Minute), dayUnix)...)

	p, good, err := decodeDvrWal(raw, testPackHash, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, len(raw), good)
	require.Equal(t, 3, p.segCount())
	require.Equal(t, 6, p.chunkCount())
	require.Len(t, p.codecs, 1)

	require.True(t, p.seg(0).Start.Equal(day))
	require.True(t, p.seg(1).Start.Equal(day.Add(2*time.Minute)))
	require.Equal(t, -1, p.findSeg(day.Add(time.Minute)))
	require.Equal(t, uint32(500_000), p.seg(2).Chunks[1].Off)

	// The deleted minute splits the day into two recording ranges.
	require.Len(t, p.ranges, 2)
	require.Equal(t, day.Unix(), p.ranges[0].From)
	require.Equal(t, int64(60), p.ranges[0].Duration)
	require.Equal(t, day.Add(2*time.Minute).Unix(), p.ranges[1].From)
	require.Equal(t, int64(120), p.ranges[1].Duration)
}

// A power cut leaves a half-written frame. Everything before it must survive,
// and the caller needs the byte count so it can truncate and keep appending.
func TestDvrWalKeepsPrefixAfterTornTail(t *testing.T) {
	day, dayUnix := testPackDay(t)

	raw := encodeDvrWalHeader(testPackHash, dayUnix)
	for i := 0; i < 3; i++ {
		raw = append(raw, encodeDvrWalSeg(dvrPackSeg{
			Start:    day.Add(time.Duration(i) * 5 * time.Second),
			Duration: 5 * time.Second,
			PTSEnd:   5 * time.Second,
			Moof:     5,
			Ready:    true,
		}, dayUnix)...)
	}
	intact := len(raw)

	torn := append([]byte(nil), raw...)
	torn = append(torn, encodeDvrWalSeg(dvrPackSeg{
		Start: day.Add(15 * time.Second), Duration: 5 * time.Second, Ready: true,
	}, dayUnix)...)
	torn = torn[:len(torn)-3]

	p, good, err := decodeDvrWal(torn, testPackHash, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, intact, good)
	require.Equal(t, 3, p.segCount())

	flipped := append([]byte(nil), raw...)
	flipped[intact-8]++
	p, good, err = decodeDvrWal(flipped, testPackHash, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, 2, p.segCount())
	require.Less(t, good, intact)

	_, _, err = decodeDvrWal(raw[:dvrWalHeaderSize], testPackHash+1, 2*time.Second)
	require.ErrorIs(t, err, errDvrPackHash)
}

// A full day of continuous recording at 5 s HLS granularity, which is the
// same 17280 entries whether the files are 5 s or 1 min long.
func benchDvrPackDay(b *testing.B, segDur, chunkDur time.Duration) *dvrPack {
	b.Helper()
	day := time.Date(2020, 1, 2, 0, 0, 0, 0, time.Local)
	p := &dvrPack{hash: testPackHash, dayUnix: day.Unix(), codecs: [][]*fmp4.InitTrack{testPackTracks()}}
	for at := time.Duration(0); at < 24*time.Hour; at += segDur {
		seg := dvrPackSeg{
			Start:    day.Add(at),
			Duration: segDur,
			PTSEnd:   at + segDur,
			Moof:     uint16(segDur / time.Second),
			CodecID:  1,
			Ready:    true,
		}
		if segDur > chunkDur {
			off := uint32(2048)
			for in := time.Duration(0); in < segDur; in += chunkDur {
				seg.Chunks = append(seg.Chunks, dvrPackChunk{
					Off:      off,
					Duration: chunkDur,
					PTSEnd:   at + in + chunkDur,
					Moof:     uint16(chunkDur / time.Second),
				})
				off += 400_000
			}
		}
		p.appendSeg(seg)
	}
	p.ranges = dvrPackRanges(p, 2*time.Second)
	return p
}

func BenchmarkDvrPackDecodeDay(b *testing.B) {
	for _, ca := range []struct {
		name   string
		segDur time.Duration
	}{
		{"seg5s", 5 * time.Second},
		{"seg1m", time.Minute},
	} {
		b.Run(ca.name, func(b *testing.B) {
			raw, err := encodeDvrPack(benchDvrPackDay(b, ca.segDur, 5*time.Second))
			require.NoError(b, err)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := decodeDvrPack(raw, testPackHash); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(len(raw))/1024, "KB/day")
		})
	}
}

// Sealing a day is a rewrite of the same records, not a conversion.
func TestDvrWalSealsIntoPack(t *testing.T) {
	day, dayUnix := testPackDay(t)

	raw := encodeDvrWalHeader(testPackHash, dayUnix)
	codec, err := encodeDvrWalCodec(1, testPackTracks())
	require.NoError(t, err)
	raw = append(raw, codec...)
	for i := 0; i < 16; i++ {
		raw = append(raw, encodeDvrWalSeg(dvrPackSeg{
			Start:    day.Add(time.Duration(i) * 5 * time.Second),
			Duration: 5 * time.Second,
			PTSEnd:   5 * time.Second,
			Moof:     5,
			CodecID:  1,
			Ready:    true,
		}, dayUnix)...)
	}

	fromWal, _, err := decodeDvrWal(raw, testPackHash, 2*time.Second)
	require.NoError(t, err)

	sealed, err := encodeDvrPack(fromWal)
	require.NoError(t, err)
	require.Less(t, len(sealed), len(raw))

	reloaded, err := decodeDvrPack(sealed, testPackHash)
	require.NoError(t, err)
	require.Equal(t, fromWal.segCount(), reloaded.segCount())
	require.Equal(t, fromWal.ranges, reloaded.ranges)
	for i := 0; i < reloaded.segCount(); i++ {
		require.Equal(t, fromWal.seg(i), reloaded.seg(i), "seg %d", i)
	}
}
