package compatapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
)

var (
	errPreviewDone     = errors.New("preview extracted")
	errNoVideoKeyframe = errors.New("no video keyframe found in segment")
	errNoVideoTrack    = errors.New("no H264/H265 video track in segment")
)

const previewSampleDuration = 90000 // 1s @ 90kHz

// ExtractPreviewMP4 reads a recording segment (mpegts or fmp4) and returns a progressive MP4 with one keyframe.
func ExtractPreviewMP4(segPath string) ([]byte, error) {
	switch {
	case strings.HasSuffix(strings.ToLower(segPath), ".mp4"):
		return ExtractPreviewFMP4(segPath)
	default:
		f, err := os.Open(segPath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return extractPreviewMP4FromReader(f)
	}
}

func extractPreviewMP4FromReader(r io.Reader) ([]byte, error) {
	mr := &mpegts.Reader{R: r}
	err := mr.Initialize()
	if err != nil {
		return nil, err
	}

	var (
		gotPreview bool
		mp4Buf     []byte
		extractErr error
	)

	setPreview := func(buf []byte, e error) error {
		mp4Buf = buf
		extractErr = e
		gotPreview = true
		return errPreviewDone
	}

	foundVideo := false

	for _, track := range mr.Tracks() {
		switch track.Codec.(type) {
		case *tscodecs.H264:
			foundVideo = true
			var sps, pps []byte
			mr.OnDataH264(track, func(_ int64, _ int64, au [][]byte) error {
				for _, nalu := range au {
					if len(nalu) == 0 {
						continue
					}
					switch h264.NALUType(nalu[0] & 0x1F) {
					case h264.NALUTypeSPS:
						sps = nalu
					case h264.NALUTypePPS:
						pps = nalu
					}
				}
				if !h264.IsRandomAccess(au) {
					return nil
				}
				if sps == nil || pps == nil {
					return nil
				}
				payload, err2 := h264.AVCC(au).Marshal()
				if err2 != nil {
					return setPreview(nil, err2)
				}
				buf, err2 := marshalSingleFrameMP4(&codecs.H264{SPS: sps, PPS: pps}, payload)
				return setPreview(buf, err2)
			})

		case *tscodecs.H265:
			foundVideo = true
			var vps, sps, pps []byte
			mr.OnDataH265(track, func(_ int64, _ int64, au [][]byte) error {
				for _, nalu := range au {
					if len(nalu) < 2 {
						continue
					}
					typ := h265.NALUType((nalu[0] >> 1) & 0b111111)
					switch typ {
					case h265.NALUType_VPS_NUT:
						vps = nalu
					case h265.NALUType_SPS_NUT:
						sps = nalu
					case h265.NALUType_PPS_NUT:
						pps = nalu
					}
				}
				if !h265.IsRandomAccess(au) {
					return nil
				}
				if vps == nil || sps == nil || pps == nil {
					return nil
				}
				payload, err2 := h264.AVCC(au).Marshal() // same length-prefixed format
				if err2 != nil {
					return setPreview(nil, err2)
				}
				buf, err2 := marshalSingleFrameMP4(&codecs.H265{VPS: vps, SPS: sps, PPS: pps}, payload)
				return setPreview(buf, err2)
			})
		}
	}

	if !foundVideo {
		return nil, errNoVideoTrack
	}

	for {
		err = mr.Read()
		if errors.Is(err, errPreviewDone) {
			break
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	if extractErr != nil {
		return nil, extractErr
	}
	if !gotPreview || len(mp4Buf) == 0 {
		return nil, errNoVideoKeyframe
	}
	return mp4Buf, nil
}

func marshalSingleFrameMP4(codec codecs.Codec, payload []byte) ([]byte, error) {
	pres := &pmp4.Presentation{
		Tracks: []*pmp4.Track{
			{
				ID:        1,
				TimeScale: 90000,
				Codec:     codec,
				Samples: []*pmp4.Sample{
					{
						Duration:    previewSampleDuration,
						PayloadSize: uint32(len(payload)),
						GetPayload: func() ([]byte, error) {
							return payload, nil
						},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	err := pres.Marshal(&buf)
	if err != nil {
		return nil, fmt.Errorf("mp4 marshal: %w", err)
	}
	return buf.Bytes(), nil
}
