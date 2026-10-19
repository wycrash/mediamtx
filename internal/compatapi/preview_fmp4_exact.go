package compatapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
)

// previewMaxGOPBytes caps the media read for one exact preview. A longer GOP
// falls back to the single keyframe.
const previewMaxGOPBytes = 4 << 20

var errPreviewGOPTooLarge = errors.New("preview GOP too large")

type previewSample struct {
	dts       int64
	duration  uint32
	ptsOffset int32
	sync      bool
	payload   []byte
}

// extractPreviewFMP4Exact returns the frame shown at `at`. The MP4 carries the
// samples from the preceding IDR up to that frame, and its edit list starts
// presentation at the frame, so the player decodes the GOP and shows only it.
func extractPreviewFMP4Exact(fpath string, parts []fmp4MediaPart, at time.Duration) ([]byte, error) {
	target := -1
	for i, p := range parts {
		if p.PTSStart > at {
			break
		}
		target = i
	}
	if target < 0 {
		return nil, errNoVideoKeyframe
	}
	start := prevIDRPart(parts, target+1)
	if start < 0 {
		return nil, errNoVideoKeyframe
	}

	_, _, init, err := readFMP4Init(fpath)
	if err != nil {
		return nil, err
	}
	var track *fmp4.InitTrack
	for _, tr := range init.Tracks {
		if tr.Codec != nil && tr.Codec.IsVideo() && tr.TimeScale != 0 {
			track = tr
			break
		}
	}
	if track == nil {
		return nil, errNoVideoTrack
	}
	atTS := int64(at) * int64(track.TimeScale) / int64(time.Second)

	f, err := os.Open(fpath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// The IDR of the start part may lie after `at`; then one earlier IDR part is needed.
	for attempt := 0; attempt < 2; attempt++ {
		samples, err := readPreviewSamples(f, parts[start:target+1], track)
		if err != nil {
			return nil, err
		}
		if mp4, ok, err := marshalExactPreview(track, samples, atTS); ok {
			return mp4, err
		}
		start = prevIDRPart(parts, start)
		if start < 0 {
			break
		}
	}
	return nil, errNoVideoKeyframe
}

func prevIDRPart(parts []fmp4MediaPart, before int) int {
	for i := before - 1; i >= 0; i-- {
		if parts[i].HasIDR {
			return i
		}
	}
	return -1
}

func readPreviewSamples(f *os.File, parts []fmp4MediaPart, track *fmp4.InitTrack) ([]previewSample, error) {
	base := parts[0].Off
	last := parts[len(parts)-1]
	size := last.Off + last.Len - base
	if size <= 0 {
		return nil, errNoVideoKeyframe
	}
	if size > previewMaxGOPBytes {
		return nil, errPreviewGOPTooLarge
	}
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, base)
	if err != nil && int64(n) != size {
		return nil, err
	}

	var out []previewSample
	for _, p := range parts {
		ss, err := parseMoofSamples(buf, p.Off-base, track)
		if err != nil {
			return nil, err
		}
		out = append(out, ss...)
	}
	return out, nil
}

// parseMoofSamples returns the video samples of the moof at buf[off:], with
// payloads sliced from the following mdat in buf.
func parseMoofSamples(buf []byte, off int64, track *fmp4.InitTrack) ([]previewSample, error) {
	if off < 0 || off+8 > int64(len(buf)) || string(buf[off+4:off+8]) != "moof" {
		return nil, fmt.Errorf("moof box not found")
	}
	moofSize := int64(binary.BigEndian.Uint32(buf[off : off+4]))
	if moofSize < 8 || off+moofSize > int64(len(buf)) {
		return nil, fmt.Errorf("bad moof size %d", moofSize)
	}

	var (
		out        []previewSample
		trackID    uint32
		baseTime   uint64
		defDur     uint32
		defFlags   uint32
		hasDefFlag bool
		parseErr   error
	)
	_, err := amp4.ReadBoxStructure(bytes.NewReader(buf[off:off+moofSize]), func(h *amp4.ReadHandle) (any, error) {
		switch h.BoxInfo.Type.String() {
		case "moof":
			return h.Expand()
		case "traf":
			trackID, baseTime, defDur, defFlags, hasDefFlag = 0, 0, 0, 0, false
			return h.Expand()
		case "tfhd":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfhd := box.(*amp4.Tfhd)
			trackID = tfhd.TrackID
			defDur = tfhd.DefaultSampleDuration
			if tfhd.GetFlags()&amp4.TfhdDefaultSampleFlagsPresent != 0 {
				defFlags = tfhd.DefaultSampleFlags
				hasDefFlag = true
			}
		case "tfdt":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			baseTime = box.(*amp4.Tfdt).GetBaseMediaDecodeTime()
		case "trun":
			if int(trackID) != track.ID {
				return nil, nil
			}
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun := box.(*amp4.Trun)
			flags := trun.GetFlags()
			sampleFlagsPresent := flags&trunSampleFlagsPresent != 0
			firstSampleFlagsPresent := flags&trunFirstSampleFlags != 0
			// mediacommon omits sample flags when every sample is sync.
			allSync := !sampleFlagsPresent && !firstSampleFlagsPresent && !hasDefFlag
			dataOff := off + int64(trun.DataOffset)
			dts := int64(baseTime)
			for i, e := range trun.Entries {
				d := e.SampleDuration
				if d == 0 {
					d = defDur
				}
				sflags, haveFlags := e.SampleFlags, sampleFlagsPresent
				if i == 0 && firstSampleFlagsPresent {
					sflags, haveFlags = trun.FirstSampleFlags, true
				}
				if !haveFlags {
					sflags, haveFlags = defFlags, hasDefFlag
				}
				sync := allSync
				if haveFlags {
					sync = !sampleIsNonSync(sflags, true)
				}
				end := dataOff + int64(e.SampleSize)
				if dataOff < 0 || end > int64(len(buf)) {
					parseErr = fmt.Errorf("sample outside of read range")
					return nil, parseErr
				}
				cts := e.SampleCompositionTimeOffsetV1
				if trun.GetVersion() == 0 {
					cts = int32(e.SampleCompositionTimeOffsetV0)
				}
				out = append(out, previewSample{
					dts:       dts,
					duration:  d,
					ptsOffset: cts,
					sync:      sync,
					payload:   buf[dataOff:end],
				})
				dts += int64(d)
				dataOff = end
			}
		}
		return nil, nil
	})
	if parseErr != nil {
		return nil, parseErr
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// marshalExactPreview reports ok=false when no IDR precedes the frame at atTS.
func marshalExactPreview(track *fmp4.InitTrack, samples []previewSample, atTS int64) ([]byte, bool, error) {
	// With B-frames decode order differs from display order: the target is the
	// latest displayed frame, wherever it sits in decode order.
	k := -1
	var kPTS int64
	for i, s := range samples {
		pts := s.dts + int64(s.ptsOffset)
		if pts <= atTS && (k < 0 || pts >= kPTS) {
			k, kPTS = i, pts
		}
	}
	if k < 0 {
		return nil, false, nil
	}
	first := -1
	for i := k; i >= 0; i-- {
		if samples[i].sync {
			first = i
			break
		}
	}
	if first < 0 {
		return nil, false, nil
	}
	if first == k {
		mp4, err := marshalSingleFrameMP4(track.Codec, samples[k].payload)
		return mp4, true, err
	}

	kept := samples[first : k+1]
	targetDTS := kept[len(kept)-1].dts - kept[0].dts
	mediaTime := targetDTS + int64(kept[len(kept)-1].ptsOffset)
	// The target is held for a second. With B-frames a reference frame decoded
	// before the target is displayed after it, so the edit list must end right
	// after the target instead.
	lastDur := int64(track.TimeScale)
	for _, s := range kept {
		if s.ptsOffset != 0 {
			lastDur = int64(kept[len(kept)-1].ptsOffset) + int64(kept[len(kept)-1].duration)
			break
		}
	}
	if mediaTime <= 0 || mediaTime > math.MaxInt32 || lastDur <= 0 || lastDur > math.MaxUint32 {
		mp4, err := marshalSingleFrameMP4(track.Codec, samples[k].payload)
		return mp4, true, err
	}

	ps := make([]*pmp4.Sample, len(kept))
	for i, s := range kept {
		payload := s.payload
		d := s.duration
		if i == len(kept)-1 {
			d = uint32(lastDur)
		}
		ps[i] = &pmp4.Sample{
			Duration:        d,
			PTSOffset:       s.ptsOffset,
			IsNonSyncSample: !s.sync,
			PayloadSize:     uint32(len(payload)),
			GetPayload: func() ([]byte, error) {
				return payload, nil
			},
		}
	}

	pres := &pmp4.Presentation{
		Tracks: []*pmp4.Track{{
			ID:         1,
			TimeScale:  track.TimeScale,
			TimeOffset: -int32(mediaTime),
			Codec:      track.Codec,
			Samples:    ps,
		}},
	}
	var out bytes.Buffer
	if err := pres.Marshal(&out); err != nil {
		return nil, true, fmt.Errorf("mp4 marshal: %w", err)
	}
	return out.Bytes(), true, nil
}
