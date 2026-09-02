package compatapi

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/playback"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

type archiveWriter struct {
	ctx      *gin.Context
	filename string
	written  bool
}

func (w *archiveWriter) Write(p []byte) (int, error) {
	if !w.written {
		w.written = true
		w.ctx.Header("Accept-Ranges", "none")
		w.ctx.Header("Content-Type", "video/mp4")
		w.ctx.Header("Content-Disposition", `inline; filename="`+w.filename+`"`)
		w.ctx.Status(http.StatusOK)
	}
	return w.ctx.Writer.Write(p)
}

func (s *Server) onArchiveMP4(ctx *gin.Context, pathName string, start time.Time, duration time.Duration) {
	if err := conf.IsValidPathName(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	if !s.doAuth(ctx, pathName) {
		return
	}
	if duration <= 0 {
		s.writeError(ctx, http.StatusBadRequest, fmt.Errorf("invalid duration"))
		return
	}
	if duration > maxArchiveDuration {
		duration = maxArchiveDuration
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

	ww := &archiveWriter{
		ctx:      ctx,
		filename: fmt.Sprintf("archive-%d-%d.mp4", start.Unix(), int64(duration/time.Second)),
	}

	err = playback.MuxAvailableSegments(pathConf.RecordFormat, segments, start, duration, "mp4", ww)
	if err != nil {
		if _, ok := errors.AsType[*net.OpError](err); ok {
			return
		}

		if !ww.written {
			if errors.Is(err, recordstore.ErrNoSegmentsFound) {
				s.writeError(ctx, http.StatusNotFound, err)
			} else {
				s.writeError(ctx, http.StatusBadRequest, err)
			}
			return
		}

		s.Log(logger.Error, err.Error())
	}
}
