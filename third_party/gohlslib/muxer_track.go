package gohlslib

import (
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
)

type muxerTrack struct {
	*Track
	variant   MuxerVariant
	stream    *muxerStream
	isLeading bool

	firstRandomAccessReceived bool
	h264DTSExtractor          *h264.DTSExtractor
	h265DTSExtractor          *h265.DTSExtractor
	h264LastDTS               int64 // remux: last written H264 DTS (clock-rate units)
	h264LastDTSInitialized    bool
	// segmentClock is a monotonic remux clock used only for HLS segment boundaries.
	// It absorbs broken PES DTS jumps so EXTINF respects SegmentMinDuration.
	segmentClock         int64
	segmentClockInit     bool
	segmentClockLastIn   int64
	segmentStartClock    int64
	mpegtsTrack          *mpegts.Track        // mpegts only
	mpegtsAACPTS         int64                // mpegts only, in track clock-rate units
	mpegtsAACPTSInitialized bool              // mpegts only
	mpegtsAudioPTS       int64                // mpegts only, MPEG-1/2 Audio continuity
	mpegtsAudioPTSInitialized bool            // mpegts only
	fmp4NextSample       *fmp4AugmentedSample // fmp4 only
	fmp4Samples          []*fmp4.Sample       // fmp4 only
	fmp4StartDTS         int64                // fmp4 only
}

func (t *muxerTrack) initialize() {
	if t.variant == MuxerVariantMPEGTS {
		t.mpegtsTrack = &mpegts.Track{
			Codec: toMPEGTS(t.Codec),
		}
	}
}
