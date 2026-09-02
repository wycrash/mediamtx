package playback

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

type writerWrapper struct {
	ctx     *gin.Context
	written bool
}

func (w *writerWrapper) Write(p []byte) (int, error) {
	if !w.written {
		w.written = true
		w.ctx.Header("Accept-Ranges", "none")
		w.ctx.Header("Content-Type", "video/mp4")
	}
	return w.ctx.Writer.Write(p)
}

func parseDuration(raw string) (time.Duration, error) {
	// seconds
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Duration(secs * float64(time.Second)), nil
	}

	// deprecated, golang format
	return time.ParseDuration(raw)
}

func muxSegmentDTS(firstMtxi *recordstore.Mtxi, startOffset time.Duration, start time.Time, seg *recordstore.Segment, init *fmp4.Init) time.Duration {
	if firstMtxi != nil {
		if mtxi := findMtxi(init.UserData); mtxi != nil && bytes.Equal(firstMtxi.StreamID[:], mtxi.StreamID[:]) {
			return time.Duration(mtxi.DTS-firstMtxi.DTS) + startOffset
		}
	}
	return seg.Start.Sub(start)
}

func seekAndMux(
	recordFormat conf.RecordFormat,
	segments []*recordstore.Segment,
	start time.Time,
	duration time.Duration,
	m muxer,
	glue bool,
) error {
	if recordFormat != conf.RecordFormatFMP4 {
		return fmt.Errorf("MPEG-TS format is not supported yet")
	}
	if len(segments) == 0 {
		return recordstore.ErrNoSegmentsFound
	}

	var firstInit *fmp4.Init
	var firstMtxi *recordstore.Mtxi
	var prevInit *fmp4.Init
	var segmentEnd time.Time
	startOffset := time.Duration(0)
	anyMuxed := false
	var opened []*os.File
	defer func() {
		for _, f := range opened {
			f.Close()
		}
	}()

	for _, seg := range segments {
		f, err := os.Open(seg.Fpath)
		if err != nil {
			if glue {
				continue
			}
			return err
		}
		opened = append(opened, f)

		init, _, err := segmentFMP4ReadHeader(f)
		if err != nil {
			if glue {
				continue
			}
			return err
		}

		var dts time.Duration
		if firstInit == nil {
			firstInit = init
			m.writeInit(&fmp4.Init{Tracks: firstInit.Tracks})
			firstMtxi = findMtxi(firstInit.UserData)
			startOffset = seg.Start.Sub(start)
			dts = startOffset
			prevInit = firstInit
		} else {
			if glue {
				if !segmentFMP4TracksAreEqual(firstInit.Tracks, init.Tracks) {
					continue
				}
			} else if !segmentFMP4CanBeConcatenated(prevInit, segmentEnd, init, seg.Start) {
				break
			}
			dts = muxSegmentDTS(firstMtxi, startOffset, start, seg, init)
			prevInit = init
		}

		segmentDuration, err := segmentFMP4MuxParts(f, dts, duration, firstInit.Tracks, m)
		if err != nil {
			if !glue {
				return err
			}
			if segmentDuration == 0 {
				if !anyMuxed {
					firstInit = nil
					prevInit = nil
					firstMtxi = nil
				}
				continue
			}
		}

		segmentEnd = seg.Start.Add(segmentDuration)
		anyMuxed = true
	}

	if !anyMuxed {
		return recordstore.ErrNoSegmentsFound
	}
	return m.flush()
}

// MuxSegments muxes recording segments into w.
// format is "fmp4" (default) or "mp4".
func MuxSegments(
	recordFormat conf.RecordFormat,
	segments []*recordstore.Segment,
	start time.Time,
	duration time.Duration,
	format string,
	w io.Writer,
) error {
	return muxSegments(recordFormat, segments, start, duration, format, w, false)
}

// MuxAvailableSegments muxes every readable segment in the window, skipping
// truncated files and gaps (non-consecutive mtxi / reconnects). Used by
// Flussonic archive-{from}-{duration}.mp4 so a broken first file does not
// drop the rest of the requested range.
func MuxAvailableSegments(
	recordFormat conf.RecordFormat,
	segments []*recordstore.Segment,
	start time.Time,
	duration time.Duration,
	format string,
	w io.Writer,
) error {
	return muxSegments(recordFormat, segments, start, duration, format, w, true)
}

func muxSegments(
	recordFormat conf.RecordFormat,
	segments []*recordstore.Segment,
	start time.Time,
	duration time.Duration,
	format string,
	w io.Writer,
	glue bool,
) error {
	var m muxer
	switch format {
	case "", "fmp4":
		m = &muxerFMP4{w: w}
	case "mp4":
		m = &muxerMP4{w: w}
	default:
		return fmt.Errorf("invalid format: %s", format)
	}
	return seekAndMux(recordFormat, segments, start, duration, m, glue)
}

func (s *Server) onGet(ctx *gin.Context) {
	pathName := ctx.Query("path")

	// validate path name before passing it to the authentication manager
	err := conf.IsValidPathName(pathName)
	if err != nil {
		s.writeError(ctx, http.StatusBadRequest, fmt.Errorf("invalid path name: %w (%s)", err, pathName))
		return
	}

	if !s.doAuth(ctx, pathName) {
		return
	}

	start, err := time.Parse(time.RFC3339, ctx.Query("start"))
	if err != nil {
		s.writeError(ctx, http.StatusBadRequest, fmt.Errorf("invalid start: %w", err))
		return
	}

	duration, err := parseDuration(ctx.Query("duration"))
	if err != nil {
		s.writeError(ctx, http.StatusBadRequest, fmt.Errorf("invalid duration: %w", err))
		return
	}

	format := ctx.Query("format")
	switch format {
	case "", "fmp4", "mp4":
	default:
		s.writeError(ctx, http.StatusBadRequest, fmt.Errorf("invalid format: %s", format))
		return
	}

	pathConf, err := s.safeFindPathConf(pathName)
	if err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	end := start.Add(duration)
	segments, err := recordstore.FindSegments(pathConf, pathName, &start, &end)
	if err != nil {
		if errors.Is(err, recordstore.ErrNoSegmentsFound) {
			s.writeError(ctx, http.StatusNotFound, err)
		} else {
			s.writeError(ctx, http.StatusBadRequest, err)
		}
		return
	}

	ww := &writerWrapper{ctx: ctx}
	err = MuxSegments(pathConf.RecordFormat, segments, start, duration, format, ww)
	if err != nil {
		// user aborted the download
		if _, ok := errors.AsType[*net.OpError](err); ok {
			return
		}

		// nothing has been written yet; send back JSON
		if !ww.written {
			if errors.Is(err, recordstore.ErrNoSegmentsFound) {
				s.writeError(ctx, http.StatusNotFound, err)
			} else {
				s.writeError(ctx, http.StatusBadRequest, err)
			}
			return
		}

		// something has already been written: abort and write logs only
		s.Log(logger.Error, err.Error())
		return
	}
}
