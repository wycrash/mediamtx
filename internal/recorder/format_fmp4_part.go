package recorder

import (
	"fmt"
	"io"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"

	"github.com/bluenviron/mediamtx/internal/conf"
)

func writePart(
	f io.Writer,
	sequenceNumber uint32,
	partTracks map[*formatFMP4Track]*fmp4.PartTrack,
) (int64, error) {
	fmp4PartTracks := make([]*fmp4.PartTrack, len(partTracks))
	i := 0
	for _, partTrack := range partTracks {
		fmp4PartTracks[i] = partTrack
		i++
	}

	part := &fmp4.Part{
		SequenceNumber: sequenceNumber,
		Tracks:         fmp4PartTracks,
	}

	var buf seekablebuffer.Buffer
	err := part.Marshal(&buf)
	if err != nil {
		return 0, err
	}

	n, err := f.Write(buf.Bytes())
	return int64(n), err
}

type formatFMP4Part struct {
	maxPartSize     conf.StringSize
	segmentStartDTS time.Duration
	number          uint32
	startDTS        time.Duration

	partTracks map[*formatFMP4Track]*fmp4.PartTrack
	size       uint64
	endDTS     time.Duration
	hasIDR     bool
}

func (p *formatFMP4Part) initialize() {
	p.partTracks = make(map[*formatFMP4Track]*fmp4.PartTrack)
}

func (p *formatFMP4Part) close(w io.Writer) (int64, error) {
	return writePart(w, p.number, p.partTracks)
}

func (p *formatFMP4Part) write(track *formatFMP4Track, sample *formatFMP4Sample, dts time.Duration) error {
	size := uint64(len(sample.Payload))
	if (p.size + size) > uint64(p.maxPartSize) {
		return fmt.Errorf("reached maximum part size")
	}
	p.size += size
	if !p.hasIDR && track.initTrack.Codec.IsVideo() && !sample.IsNonSyncSample {
		p.hasIDR = true
	}

	partTrack, ok := p.partTracks[track]
	if !ok {
		partTrack = &fmp4.PartTrack{
			ID: track.initTrack.ID,
			BaseTime: uint64(multiplyAndDivide(int64(dts-p.segmentStartDTS),
				int64(track.initTrack.TimeScale), int64(time.Second))),
		}
		p.partTracks[track] = partTrack
	}

	partTrack.Samples = append(partTrack.Samples, sample.Sample)

	endDTS := dts + timestampToDuration(int64(sample.Duration), int(track.initTrack.TimeScale))
	if endDTS > p.endDTS {
		p.endDTS = endDTS
	}

	return nil
}

func (p *formatFMP4Part) duration() time.Duration {
	return p.endDTS - p.startDTS
}
