package gohlslib

import (
	"testing"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
	"github.com/stretchr/testify/require"
)

func TestAdvanceSegmentClock(t *testing.T) {
	track := &muxerTrack{
		Track: &Track{
			Codec:     &codecs.H264{},
			ClockRate: 90000,
		},
	}

	c0 := advanceSegmentClock(track, 1_000_000)
	require.Equal(t, int64(1_000_000), c0)

	// Normal 40ms step is accepted.
	c1 := advanceSegmentClock(track, 1_000_000+3600)
	require.Equal(t, int64(1_000_000+3600), c1)

	// Huge jump is replaced with a default 40ms step.
	c2 := advanceSegmentClock(track, 1_000_000+3600+90000*10)
	require.Equal(t, c1+3600, c2)

	// Backward / non-monotonic input also falls back to default step.
	c3 := advanceSegmentClock(track, 1_000_000)
	require.Equal(t, c2+3600, c3)
}

func TestMPEGTSSegmentMinDurationGuard(t *testing.T) {
	videoTrack := &Track{
		Codec: &codecs.H264{
			SPS: []byte{
				0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02,
				0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04,
				0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9, 0x20,
			},
			PPS: []byte{0x08},
		},
		ClockRate: 90000,
	}

	m := &Muxer{
		Variant:            MuxerVariantMPEGTS,
		SegmentCount:       3,
		SegmentMinDuration: time.Second,
		Tracks:             []*Track{videoTrack},
	}
	require.NoError(t, m.Start())
	defer m.Close()

	ntp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idr := [][]byte{{5, 1, 2, 3, 4}} // IDR NALU

	// First IDR creates the segment.
	require.NoError(t, m.WriteH264WithDTS(videoTrack, ntp, 90000, 90000, idr))

	// Another IDR 400ms later must NOT close the segment.
	require.NoError(t, m.WriteH264WithDTS(videoTrack, ntp.Add(400*time.Millisecond), 90000+36000, 90000+36000, idr))

	m.mutex.Lock()
	seg := m.leadingStream.nextSegment.(*muxerSegmentMPEGTS)
	require.Equal(t, uint64(0), seg.id)
	require.Equal(t, 0, len(m.leadingStream.segments))
	m.mutex.Unlock()

	// After >= 1s media+wall time, IDR closes the segment.
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, m.WriteH264WithDTS(videoTrack, ntp.Add(1200*time.Millisecond), 90000+108000, 90000+108000, idr))

	m.mutex.Lock()
	require.Equal(t, 1, len(m.leadingStream.segments))
	closed := m.leadingStream.segments[0]
	require.GreaterOrEqual(t, closed.getDuration(), time.Second)
	m.mutex.Unlock()
}
