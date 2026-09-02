package recorder

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/bluenviron/mediamtx/internal/stream"
)

type recorderInstance struct {
	pathFormat        string
	format            conf.RecordFormat
	partDuration      time.Duration
	maxPartSize       conf.StringSize
	segmentDuration   time.Duration
	pathName          string
	stream            *stream.Stream
	onSegmentCreate   OnSegmentCreateFunc
	onSegmentComplete OnSegmentCompleteFunc
	pickRoot          PickRootFunc
	parent            logger.Writer

	streamID    uuid.UUID
	pathFormat2 string
	format2     format
	skip        bool
	reader      *stream.Reader

	terminate chan struct{}
	done      chan struct{}
}

// Log implements logger.Writer.
func (ri *recorderInstance) Log(level logger.Level, format string, args ...any) {
	ri.parent.Log(level, format, args...)
}

func (ri *recorderInstance) initialize() {
	ri.streamID = uuid.New()
	format := ri.pathFormat
	if ri.pickRoot != nil {
		format = conf.RecordPathRel(ri.pathFormat)
	}
	ri.pathFormat2 = recordstore.PathAddExtension(
		strings.ReplaceAll(format, "%path", ri.pathName),
		ri.format,
	)
	ri.reader = &stream.Reader{
		SkipOutboundBytes: true,
		Parent:            ri,
	}

	ri.terminate = make(chan struct{})
	ri.done = make(chan struct{})

	switch ri.format {
	case conf.RecordFormatMPEGTS:
		ri.format2 = &formatMPEGTS{
			ri: ri,
		}
		ok := ri.format2.initialize()
		ri.skip = !ok

	default:
		ri.format2 = &formatFMP4{
			ri: ri,
		}
		ok := ri.format2.initialize()
		ri.skip = !ok
	}

	if !ri.skip {
		ri.stream.AddReader(ri.reader)
	}

	go ri.run()
}

func (ri *recorderInstance) close() {
	close(ri.terminate)
	<-ri.done
}

func (ri *recorderInstance) run() {
	defer close(ri.done)

	if !ri.skip {
		select {
		case err := <-ri.reader.Error():
			ri.Log(logger.Error, err.Error())

		case <-ri.terminate:
		}

		ri.stream.RemoveReader(ri.reader)
	} else {
		<-ri.terminate
	}

	ri.format2.close()
}

func (ri *recorderInstance) createSegmentFile(start time.Time) (*os.File, string, error) {
	var skip []string
	for {
		format := ri.pathFormat2
		if ri.pickRoot != nil {
			root, err := ri.pickRoot(skip)
			if err != nil {
				return nil, "", err
			}
			format = filepath.Join(root, ri.pathFormat2)
			skip = append(skip, root)
		}
		path := recordstore.Path{Start: start}.Encode(format)
		err := os.MkdirAll(filepath.Dir(path), 0o755)
		if err != nil {
			if ri.pickRoot != nil && isNoSpace(err) {
				continue
			}
			return nil, "", err
		}
		fi, err := os.Create(path)
		if err != nil {
			if ri.pickRoot != nil && isNoSpace(err) {
				continue
			}
			return nil, "", err
		}
		return fi, path, nil
	}
}
