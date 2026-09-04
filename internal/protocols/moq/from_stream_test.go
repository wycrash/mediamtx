package moq

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/flac"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/protocols/moq/catalog"
)

func TestFromStream(t *testing.T) {
	flacEnc, err := (&flac.StreamInfo{
		SampleRate:   44100,
		ChannelCount: 2,
		BitDepth:     16,
	}).Marshal()
	require.NoError(t, err)

	mpeg4Config := &mpeg4audio.AudioSpecificConfig{
		Type:          2,
		SampleRate:    44100,
		ChannelConfig: 2,
	}
	mpeg4Enc, err := mpeg4Config.Marshal()
	require.NoError(t, err)

	desc := &description.Session{
		Medias: []*description.Media{
			{Formats: []format.Format{&format.AV1{}}},
			{Formats: []format.Format{&format.VP9{}}},
			{Formats: []format.Format{&format.VP8{}}},
			{Formats: []format.Format{&format.H265{}}},
			{Formats: []format.Format{&format.H264{}}},
			{Formats: []format.Format{&format.Opus{ChannelCount: 2}}},
			{Formats: []format.Format{&format.Generic{
				RTPMa:    "FLAC/44100/2",
				ClockRat: 44100,
				FMT:      map[string]string{"streaminfo": hex.EncodeToString(flacEnc)},
			}}},
			{Formats: []format.Format{&format.MPEG4Audio{
				Config:           mpeg4Config,
				SizeLength:       13,
				IndexLength:      3,
				IndexDeltaLength: 3,
			}}},
			{Formats: []format.Format{&format.G711{
				SampleRate:   8000,
				ChannelCount: 1,
			}}},
			{Formats: []format.Format{&format.LPCM{
				BitDepth:     16,
				SampleRate:   44100,
				ChannelCount: 2,
			}}},
		},
	}

	cat, _, err := FromStream(desc, desc)
	require.NoError(t, err)

	require.Equal(t, &catalog.Catalog{
		Version: 1,
		Tracks: []catalog.Track{
			{
				Name:      "0",
				Packaging: "loc",
				IsLive:    true,
				Codec:     "av01.0.04M.08",
				ClockRate: 90000,
			},
			{
				Name:      "1",
				Packaging: "loc",
				IsLive:    true,
				Codec:     "vp09.00.10.08",
				ClockRate: 90000,
			},
			{
				Name:      "2",
				Packaging: "loc",
				IsLive:    true,
				Codec:     "vp8",
				ClockRate: 90000,
			},
			{
				Name:      "3",
				Packaging: "loc",
				IsLive:    true,
				Codec:     "hev1.1.6.L93.B0",
				ClockRate: 90000,
			},
			{
				Name:      "4",
				Packaging: "loc",
				IsLive:    true,
				Codec:     "avc3.640028",
				ClockRate: 90000,
			},
			{
				Name:       "5",
				Packaging:  "loc",
				IsLive:     true,
				Codec:      "opus",
				Samplerate: 48000,
				Channels:   2,
				ClockRate:  48000,
			},
			{
				Name:       "6",
				Packaging:  "loc",
				IsLive:     true,
				Codec:      "flac",
				Samplerate: 44100,
				Channels:   2,
				ClockRate:  44100,
				InitData:   base64.StdEncoding.EncodeToString(flacEnc),
			},
			{
				Name:       "7",
				Packaging:  "loc",
				IsLive:     true,
				Codec:      "mp4a.40.2",
				Samplerate: 44100,
				Channels:   2,
				ClockRate:  44100,
				InitData:   base64.StdEncoding.EncodeToString(mpeg4Enc),
			},
			{
				Name:       "8",
				Packaging:  "loc",
				IsLive:     true,
				Codec:      "pcm-s16",
				Samplerate: 8000,
				Channels:   1,
				ClockRate:  8000,
			},
			{
				Name:       "9",
				Packaging:  "loc",
				IsLive:     true,
				Codec:      "pcm-s16",
				Samplerate: 44100,
				Channels:   2,
				ClockRate:  44100,
			},
		},
	}, cat)
}

func TestFromStreamH264InitData(t *testing.T) {
	desc := &description.Session{
		Medias: []*description.Media{{
			Formats: []format.Format{&format.H264{
				PayloadTyp:        96,
				SPS:               []byte{0x67, 0x42, 0xc0, 0x28, 0x00},
				PPS:               []byte{0x08, 0x06, 0x07, 0x08},
				PacketizationMode: 1,
			}},
		}},
	}

	cat, _, err := FromStream(desc, desc)
	require.NoError(t, err)
	require.Equal(t, "avc1.42c028", cat.Tracks[0].Codec)
	require.Equal(t, 90000, cat.Tracks[0].ClockRate)
	require.NotEmpty(t, cat.Tracks[0].InitData)

	avcC, err := base64.StdEncoding.DecodeString(cat.Tracks[0].InitData)
	require.NoError(t, err)
	sps, pps, err := parseAvcC(avcC)
	require.NoError(t, err)
	require.Equal(t, []byte{0x67, 0x42, 0xc0, 0x28, 0x00}, sps)
	require.Equal(t, []byte{0x08, 0x06, 0x07, 0x08}, pps)
}
