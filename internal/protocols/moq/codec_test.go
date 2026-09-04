package moq

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

func TestMakeParseAvcC(t *testing.T) {
	sps := test.FormatH264.SPS
	pps := test.FormatH264.PPS
	avcC := makeAvcC(sps, pps)
	gotSPS, gotPPS, err := parseAvcC(avcC)
	require.NoError(t, err)
	require.Equal(t, sps, gotSPS)
	require.Equal(t, pps, gotPPS)
	require.Equal(t, "avc1.42c028", h264CodecString(sps))
}

func TestMakeParseHvcC(t *testing.T) {
	vps := test.FormatH265.VPS
	sps := test.FormatH265.SPS
	pps := test.FormatH265.PPS
	hvcC := makeHvcC(vps, sps, pps)
	gotVPS, gotSPS, gotPPS, err := parseHvcC(hvcC)
	require.NoError(t, err)
	require.Equal(t, vps, gotVPS)
	require.Equal(t, sps, gotSPS)
	require.Equal(t, pps, gotPPS)
	require.True(t, len(h265CodecString(sps)) > 5)
	require.Equal(t, "hvc1", h265CodecString(sps)[:4])
}

func TestStripH264Params(t *testing.T) {
	au := [][]byte{
		test.FormatH264.SPS,
		test.FormatH264.PPS,
		{9, 0x10}, // AUD
		{5, 1},
	}
	require.Equal(t, [][]byte{{5, 1}}, stripH264Params(au))
}
