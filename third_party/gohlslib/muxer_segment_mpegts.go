package gohlslib

import (
	"bufio"
	"fmt"
	"io"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg1audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"

	"github.com/bluenviron/gohlslib/v2/pkg/storage"
)

type muxerSegmentMPEGTS struct {
	segmentMaxSize uint64
	prefix         string
	storageFactory storage.Factory
	streamID       string
	mpegtsWriter   *mpegts.Writer
	id             uint64
	startNTP       time.Time
	startWall      time.Time
	startDTS       time.Duration

	storage     storage.File
	storagePart storage.Part
	bw          *bufio.Writer
	size        uint64
	path        string
	endDTS      time.Duration // available after finalize()
}

func (s *muxerSegmentMPEGTS) initialize() error {
	s.path = segmentPath(s.prefix, s.streamID, s.id, false)
	s.startWall = time.Now()

	var err error
	s.storage, err = s.storageFactory.NewFile(s.path)
	if err != nil {
		return err
	}

	s.storagePart = s.storage.NewPart()
	s.bw = bufio.NewWriter(s.storagePart.Writer())

	return nil
}

func (s *muxerSegmentMPEGTS) close() {
	s.storage.Remove()
}

func (s *muxerSegmentMPEGTS) getPath() string {
	return s.path
}

func (s *muxerSegmentMPEGTS) getDuration() time.Duration {
	return s.endDTS - s.startDTS
}

func (s *muxerSegmentMPEGTS) getSize() uint64 {
	return s.storage.Size()
}

func (s *muxerSegmentMPEGTS) reader() (io.ReadCloser, error) {
	return s.storage.Reader()
}

func (s *muxerSegmentMPEGTS) finalize(endDTS time.Duration) error {
	err := s.bw.Flush()
	if err != nil {
		return err
	}

	s.bw = nil
	s.storage.Finalize()
	s.endDTS = endDTS

	return nil
}

func (s *muxerSegmentMPEGTS) writeH264(
	track *muxerTrack,
	pts int64,
	dts int64,
	au [][]byte,
) error {
	size := uint64(0)
	for _, nalu := range au {
		size += uint64(len(nalu))
	}
	if (s.size + size) > s.segmentMaxSize {
		return fmt.Errorf("reached maximum segment size")
	}
	s.size += size

	pts90 := multiplyAndDivide(pts, 90000, int64(track.ClockRate))
	dts90 := multiplyAndDivide(dts, 90000, int64(track.ClockRate))
	pts90, dts90 = track.stream.mpegtsRebase(pts90, dts90)

	err := s.mpegtsWriter.WriteH264(
		track.mpegtsTrack,
		pts90,
		dts90,
		au,
	)
	if err != nil {
		return err
	}

	return nil
}

func (s *muxerSegmentMPEGTS) writeMPEG4Audio(
	track *muxerTrack,
	pts int64,
	aus [][]byte,
) error {
	size := uint64(0)
	for _, au := range aus {
		size += uint64(len(au))
	}

	if (s.size + size) > s.segmentMaxSize {
		return fmt.Errorf("reached maximum segment size")
	}
	s.size += size

	tolerance := durationToTimestamp(mpegtsAACPTSDriftTolerance, track.ClockRate)

	// recompute timestamp from scratch.
	// iOS+MPEG-TS+AAC requires a precise timestamp that might get lost during timestamp conversion.
	// also reset in case of drifts.
	if !track.mpegtsAACPTSInitialized ||
		track.mpegtsAACPTS > (pts+tolerance) ||
		track.mpegtsAACPTS < (pts-tolerance) {
		track.mpegtsAACPTS = pts
		track.mpegtsAACPTSInitialized = true
	}

	err := s.mpegtsWriter.WriteMPEG4Audio(
		track.mpegtsTrack,
		func() int64 {
			pts90 := multiplyAndDivide(track.mpegtsAACPTS, 90000, int64(track.ClockRate))
			pts90, _ = track.stream.mpegtsRebase(pts90, pts90)
			return pts90
		}(),
		aus,
	)
	if err != nil {
		return err
	}

	track.mpegtsAACPTS += mpeg4audio.SamplesPerAccessUnit * int64(len(aus))

	return nil
}

func (s *muxerSegmentMPEGTS) writeMPEG1Audio(
	track *muxerTrack,
	pts int64,
	frames [][]byte,
) error {
	size := uint64(0)
	for _, frame := range frames {
		size += uint64(len(frame))
	}

	if (s.size + size) > s.segmentMaxSize {
		return fmt.Errorf("reached maximum segment size")
	}
	s.size += size

	// Lock audio to the leading video DTS/PCR timeline. Free-running audio PTS
	// is what made VLC report "dts later than pcr" and reset the demuxer.
	tolerance := durationToTimestamp(mpegtsAACPTSDriftTolerance, track.ClockRate)
	if track.stream.mpegtsLeadingDTSInit {
		lead := track.stream.mpegtsLeadingDTS
		if pts > lead+tolerance {
			pts = lead
		} else if pts < lead-tolerance {
			pts = lead
		}
	}

	if !track.mpegtsAudioPTSInitialized ||
		track.mpegtsAudioPTS > (pts+tolerance) ||
		track.mpegtsAudioPTS < (pts-tolerance) {
		track.mpegtsAudioPTS = pts
		track.mpegtsAudioPTSInitialized = true
	}

	pts90 := multiplyAndDivide(track.mpegtsAudioPTS, 90000, int64(track.ClockRate))
	pts90, _ = track.stream.mpegtsRebase(pts90, pts90)

	err := s.mpegtsWriter.WriteMPEG1Audio(
		track.mpegtsTrack,
		pts90,
		frames,
	)
	if err != nil {
		return err
	}

	samplesPerFrame := int64(1152)
	sampleRate := int64(48000)
	if len(frames) > 0 {
		var h mpeg1audio.FrameHeader
		if h.Unmarshal(frames[0]) == nil && h.SampleRate > 0 {
			samplesPerFrame = int64(h.SampleCount())
			sampleRate = int64(h.SampleRate)
		}
	}
	track.mpegtsAudioPTS += multiplyAndDivide(
		samplesPerFrame*int64(len(frames)),
		int64(track.ClockRate),
		sampleRate,
	)

	return nil
}

func (s *muxerSegmentMPEGTS) writeKLV(
	track *muxerTrack,
	pts int64,
	data []byte,
) error {
	size := uint64(len(data))
	if (s.size + size) > s.segmentMaxSize {
		return fmt.Errorf("reached maximum segment size")
	}
	s.size += size

	return s.mpegtsWriter.WriteKLV(
		track.mpegtsTrack,
		pts,
		data,
	)
}
