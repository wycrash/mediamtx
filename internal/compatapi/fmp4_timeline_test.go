package compatapi

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/stretchr/testify/require"
)

func TestInspectFMP4SegmentReadsDurationAndMoofCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seg.mp4")
	f, err := os.Create(path)
	require.NoError(t, err)

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
	require.NoError(t, (fmp4.Part{
		SequenceNumber: 0,
		Tracks: []*fmp4.PartTrack{{
			ID:       1,
			BaseTime: 0,
			Samples:  []*fmp4.Sample{{Duration: 1000, Payload: []byte{1, 2, 3, 4}}},
		}},
	}).Marshal(f))
	require.NoError(t, (fmp4.Part{
		SequenceNumber: 1,
		Tracks: []*fmp4.PartTrack{{
			ID:       1,
			BaseTime: 1000,
			Samples:  []*fmp4.Sample{{Duration: 1000, Payload: []byte{5, 6, 7, 8}}},
		}},
	}).Marshal(f))
	require.NoError(t, f.Close())

	meta, err := inspectFMP4Segment(path)
	require.NoError(t, err)
	require.True(t, meta.Ready)
	require.Equal(t, uint32(2), meta.MoofCount)
	require.Equal(t, 2*time.Second, meta.Duration)
	require.NotEmpty(t, meta.Tracks)

	initSize, err := fmp4InitSize(path)
	require.NoError(t, err)
	require.Greater(t, initSize, int64(0))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	media := append([]byte(nil), raw[initSize:]...)
	require.NoError(t, patchFMP4MediaTimeline(media, 10, map[uint32]uint64{1: 5000}))

	i := bytes.Index(media, []byte("mfhd"))
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, uint32(10), binary.BigEndian.Uint32(media[i+8:i+12]))
	i = bytes.Index(media, []byte("tfdt"))
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, uint64(5000), binary.BigEndian.Uint64(media[i+8:i+16]))
}
