// Package moq contains Media-over-QUIC utilities.
package moq

import (
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/av1"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/flac"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/g711"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/vp8"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/vp9"

	"github.com/bluenviron/mediamtx/internal/protocols/moq/catalog"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
)

type writeDataFunc func(payload []byte, pts int64) error

// SetupTrackFunc is a function that sets up a track in a MediaMTX stream.
type SetupTrackFunc func(r *stream.Reader, writeData writeDataFunc)

func outFormatAt(outDesc *description.Session, mediaIdx, formatIdx int, orig format.Format) format.Format {
	if outDesc == nil || mediaIdx >= len(outDesc.Medias) {
		return orig
	}
	outMedia := outDesc.Medias[mediaIdx]
	if formatIdx >= len(outMedia.Formats) {
		return orig
	}
	return outMedia.Formats[formatIdx]
}

// FromStream maps a MediaMTX stream to a Media-over-QUIC catalog and subscribed tracks.
// origDesc is used to bind readers; outDesc (when non-nil) supplies live codec parameters.
func FromStream(origDesc, outDesc *description.Session) (*catalog.Catalog, []SetupTrackFunc, error) {
	if outDesc == nil {
		outDesc = origDesc
	}

	cat := &catalog.Catalog{
		Version: 1,
	}

	var setupTracks []SetupTrackFunc

	addTrack := func(
		media *description.Media,
		forma format.Format,
		track catalog.Track,
		genParsePayload func(writeData writeDataFunc) func(u *unit.Unit) error,
	) {
		track.Name = strconv.Itoa(len(cat.Tracks))
		track.Packaging = "loc"
		track.IsLive = true
		if track.ClockRate == 0 {
			track.ClockRate = forma.ClockRate()
		}

		setup := func(r *stream.Reader, writeData writeDataFunc) {
			parsePayload := genParsePayload(writeData)

			r.OnData(media, forma, func(u *unit.Unit) error {
				return parsePayload(u)
			})
		}

		cat.Tracks = append(cat.Tracks, track)
		setupTracks = append(setupTracks, setup)
	}

	for mediaIdx, media := range origDesc.Medias {
		for formatIdx, forma := range media.Formats {
			switch forma := forma.(type) {
			case *format.AV1:
				addTrack(
					media,
					forma,
					catalog.Track{
						Codec:     "av01.0.04M.08",
						ClockRate: forma.ClockRate(),
					},
					func(writeData writeDataFunc) func(u *unit.Unit) error {
						firstRandomAccess := false

						return func(u *unit.Unit) error {
							if u.NilPayload() {
								return nil
							}

							if !firstRandomAccess && !av1.IsRandomAccess2(u.Payload.(unit.PayloadAV1)) {
								return nil
							}
							firstRandomAccess = true

							payload, err := av1.Bitstream([][]byte(u.Payload.(unit.PayloadAV1))).Marshal()
							if err != nil {
								return err
							}

							return writeData(payload, u.PTS)
						}
					},
				)

			case *format.VP9:
				addTrack(
					media,
					forma,
					catalog.Track{
						Codec:     "vp09.00.10.08",
						ClockRate: forma.ClockRate(),
					},
					func(writeData writeDataFunc) func(u *unit.Unit) error {
						firstRandomAccess := false

						return func(u *unit.Unit) error {
							if u.NilPayload() {
								return nil
							}

							if !firstRandomAccess && !vp9.IsRandomAccess(u.Payload.(unit.PayloadVP9)) {
								return nil
							}
							firstRandomAccess = true

							return writeData(u.Payload.(unit.PayloadVP9), u.PTS)
						}
					},
				)

			case *format.VP8:
				addTrack(
					media,
					forma,
					catalog.Track{
						Codec:     "vp8",
						ClockRate: forma.ClockRate(),
					},
					func(writeData writeDataFunc) func(u *unit.Unit) error {
						firstRandomAccess := false

						return func(u *unit.Unit) error {
							if u.NilPayload() {
								return nil
							}

							if !firstRandomAccess && !vp8.IsRandomAccess(u.Payload.(unit.PayloadVP8)) {
								return nil
							}
							firstRandomAccess = true

							return writeData(u.Payload.(unit.PayloadVP8), u.PTS)
						}
					},
				)

			case *format.H265:
				outH265, _ := outFormatAt(outDesc, mediaIdx, formatIdx, forma).(*format.H265)
				if outH265 == nil {
					outH265 = forma
				}
				h265Track := catalog.Track{
					Codec:     "hev1.1.6.L93.B0",
					ClockRate: forma.ClockRate(),
				}
				stripH265 := false
				if len(outH265.VPS) > 0 && len(outH265.SPS) >= 15 && len(outH265.PPS) > 0 {
					h265Track.Codec = h265CodecString(outH265.SPS)
					h265Track.InitData = base64.StdEncoding.EncodeToString(
						makeHvcC(outH265.VPS, outH265.SPS, outH265.PPS))
					stripH265 = true
				}
				addTrack(
					media,
					forma,
					h265Track,
					func(writeData writeDataFunc) func(u *unit.Unit) error {
						firstRandomAccess := false

						return func(u *unit.Unit) error {
							if u.NilPayload() {
								return nil
							}

							nalus := [][]byte(u.Payload.(unit.PayloadH265))
							if !firstRandomAccess && !h265.IsRandomAccess(nalus) {
								return nil
							}
							firstRandomAccess = true

							if stripH265 {
								nalus = stripH265Params(nalus)
								if len(nalus) == 0 {
									return nil
								}
							}

							payload, err := h264.AVCC(nalus).Marshal()
							if err != nil {
								return err
							}

							return writeData(payload, u.PTS)
						}
					},
				)

			case *format.H264:
				outH264, _ := outFormatAt(outDesc, mediaIdx, formatIdx, forma).(*format.H264)
				if outH264 == nil {
					outH264 = forma
				}
				h264Track := catalog.Track{
					Codec:     "avc3.640028",
					ClockRate: forma.ClockRate(),
				}
				stripH264 := false
				if len(outH264.SPS) >= 4 && len(outH264.PPS) > 0 {
					h264Track.Codec = h264CodecString(outH264.SPS)
					h264Track.InitData = base64.StdEncoding.EncodeToString(makeAvcC(outH264.SPS, outH264.PPS))
					stripH264 = true
				}
				addTrack(
					media,
					forma,
					h264Track,
					func(writeData writeDataFunc) func(u *unit.Unit) error {
						firstRandomAccess := false

						return func(u *unit.Unit) error {
							if u.NilPayload() {
								return nil
							}

							nalus := [][]byte(u.Payload.(unit.PayloadH264))
							if !firstRandomAccess && !h264.IsRandomAccess(nalus) {
								return nil
							}
							firstRandomAccess = true

							if stripH264 {
								nalus = stripH264Params(nalus)
								if len(nalus) == 0 {
									return nil
								}
							}

							payload, err := h264.AVCC(nalus).Marshal()
							if err != nil {
								return err
							}

							return writeData(payload, u.PTS)
						}
					},
				)

			case *format.Opus:
				addTrack(
					media,
					forma,
					catalog.Track{
						Codec:      "opus",
						Samplerate: 48000,
						Channels:   forma.ChannelCount,
						ClockRate:  forma.ClockRate(),
					},
					func(writeData writeDataFunc) func(u *unit.Unit) error {
						return func(u *unit.Unit) error {
							if u.NilPayload() {
								return nil
							}

							for _, pkt := range u.Payload.(unit.PayloadOpus) {
								err := writeData(pkt, u.PTS)
								if err != nil {
									return err
								}
							}
							return nil
						}
					},
				)

			case *format.Generic:
				if strings.HasPrefix(strings.ToLower(forma.RTPMap()), "flac/") {
					enc, err := hex.DecodeString(forma.FMT["streaminfo"])
					if err != nil {
						return nil, nil, err
					}

					var streamInfo flac.StreamInfo
					err = streamInfo.Unmarshal(enc)
					if err != nil {
						return nil, nil, err
					}

					addTrack(
						media,
						forma,
						catalog.Track{
							Codec:      "flac",
							Samplerate: int(streamInfo.SampleRate),
							Channels:   int(streamInfo.ChannelCount),
							ClockRate:  forma.ClockRate(),
							InitData:   base64.StdEncoding.EncodeToString(enc),
						},
						func(writeData writeDataFunc) func(u *unit.Unit) error {
							return func(u *unit.Unit) error {
								if u.NilPayload() {
									return nil
								}

								return writeData(u.Payload.(unit.PayloadFLAC), u.PTS)
							}
						},
					)
				}

			case *format.MPEG4Audio:
				if forma.Config != nil {
					enc, err := forma.Config.Marshal()
					if err != nil {
						return nil, nil, err
					}

					addTrack(
						media,
						forma,
						catalog.Track{
							Codec:      "mp4a.40.2",
							Samplerate: forma.Config.SampleRate,
							Channels:   int(forma.Config.ChannelConfig),
							ClockRate:  forma.ClockRate(),
							InitData:   base64.StdEncoding.EncodeToString(enc),
						},
						func(writeData writeDataFunc) func(u *unit.Unit) error {
							return func(u *unit.Unit) error {
								if u.NilPayload() {
									return nil
								}

								pts := u.PTS

								for _, au := range u.Payload.(unit.PayloadMPEG4Audio) {
									err2 := writeData(au, pts)
									if err2 != nil {
										return err2
									}

									pts += mpeg4audio.SamplesPerAccessUnit
								}
								return nil
							}
						},
					)
				}

			case *format.G711:
				addTrack(
					media,
					forma,
					catalog.Track{
						Codec:      "pcm-s16",
						Samplerate: forma.SampleRate,
						Channels:   forma.ChannelCount,
						ClockRate:  forma.ClockRate(),
					},
					func(writeData writeDataFunc) func(u *unit.Unit) error {
						return func(u *unit.Unit) error {
							if u.NilPayload() {
								return nil
							}

							var bigEndian []byte
							if forma.MULaw {
								var mu g711.Mulaw
								mu.Unmarshal(u.Payload.(unit.PayloadG711))
								bigEndian = mu
							} else {
								var al g711.Alaw
								al.Unmarshal(u.Payload.(unit.PayloadG711))
								bigEndian = al
							}

							swapped := make([]byte, len(bigEndian))
							for i := 0; i+2 <= len(bigEndian); i += 2 {
								swapped[i], swapped[i+1] = bigEndian[i+1], bigEndian[i]
							}

							return writeData(swapped, u.PTS)
						}
					},
				)

			case *format.LPCM:
				var codec string
				switch forma.BitDepth {
				case 8:
					codec = "pcm-u8"
				case 16:
					codec = "pcm-s16"
				case 24:
					codec = "pcm-s24"
				default: // 32
					codec = "pcm-s32"
				}

				addTrack(
					media,
					forma,
					catalog.Track{
						Codec:      codec,
						Samplerate: forma.SampleRate,
						Channels:   forma.ChannelCount,
						ClockRate:  forma.ClockRate(),
					},
					func(writeData writeDataFunc) func(u *unit.Unit) error {
						return func(u *unit.Unit) error {
							if u.NilPayload() {
								return nil
							}

							src := []byte(u.Payload.(unit.PayloadLPCM))
							byteDepth := forma.BitDepth / 8
							swapped := make([]byte, len(src))
							for i := 0; i+byteDepth <= len(src); i += byteDepth {
								for j := range byteDepth {
									swapped[i+j] = src[i+byteDepth-1-j]
								}
							}

							return writeData(swapped, u.PTS)
						}
					},
				)
			}
		}
	}

	return cat, setupTracks, nil
}
