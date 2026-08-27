package compatapi

import (
	"fmt"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

// hlsMSECodecOK reports whether Chrome MSE / hls.js can play this fMP4 codec.
// Matches the live HLS muxer: AV1, VP9, H265, H264, Opus, MPEG-4 Audio.
// LPCM (ipcm) plays in a standalone MP4 but MSE rejects it, so archive HLS
// would fail even though the same file plays when opened directly.
func hlsMSECodecOK(c mcodecs.Codec) bool {
	switch c.(type) {
	case *mcodecs.AV1, *mcodecs.VP9, *mcodecs.H265, *mcodecs.H264,
		*mcodecs.Opus, *mcodecs.MPEG4Audio:
		return true
	default:
		return false
	}
}

func hlsDropTrackIDs(init *fmp4.Init) map[uint32]struct{} {
	drop := make(map[uint32]struct{})
	if init == nil {
		return drop
	}
	for _, tr := range init.Tracks {
		if tr == nil || !hlsMSECodecOK(tr.Codec) {
			if tr != nil {
				drop[uint32(tr.ID)] = struct{}{}
			}
		}
	}
	return drop
}

// marshalFMP4InitForHLS rewrites moov without MSE-incompatible tracks.
// A nil slice means the original init bytes can be served as-is.
func marshalFMP4InitForHLS(init *fmp4.Init, drop map[uint32]struct{}) ([]byte, error) {
	if init == nil || len(drop) == 0 {
		return nil, nil
	}
	kept := make([]*fmp4.InitTrack, 0, len(init.Tracks))
	for _, tr := range init.Tracks {
		if tr == nil {
			continue
		}
		if _, skip := drop[uint32(tr.ID)]; skip {
			continue
		}
		kept = append(kept, tr)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("no HLS-compatible tracks in init")
	}
	out := fmp4.Init{Tracks: kept, UserData: init.UserData}
	var buf seekablebuffer.Buffer
	if err := out.Marshal(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func rewriteFMP4MediaForHLS(
	media []byte,
	drop map[uint32]struct{},
	startSeq uint32,
	trackOffsets map[uint32]uint64,
) ([]byte, error) {
	var parts fmp4.Parts
	if err := parts.Unmarshal(media); err != nil {
		return nil, err
	}
	seq := startSeq
	for _, p := range parts {
		p.SequenceNumber = seq
		seq++
		kept := p.Tracks[:0]
		for _, tr := range p.Tracks {
			if _, skip := drop[uint32(tr.ID)]; skip {
				continue
			}
			if off := trackOffsets[uint32(tr.ID)]; off > 0 {
				tr.BaseTime += off
			}
			kept = append(kept, tr)
		}
		p.Tracks = kept
		if len(p.Tracks) == 0 {
			return nil, fmt.Errorf("no HLS-compatible tracks in fragment")
		}
	}
	var buf seekablebuffer.Buffer
	if err := parts.Marshal(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
