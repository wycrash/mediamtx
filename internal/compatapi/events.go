package compatapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/eventstore"
)

type eventSpanJSON struct {
	From     int64  `json:"from"`
	Duration int64  `json:"duration"`
	Class    string `json:"class"`
	Topic    string `json:"topic"`
	Source   string `json:"source"`
}

func eventSortDesc(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "asc":
		return false
	default:
		return true
	}
}

func (s *Server) onEventsJSON(ctx *gin.Context, pathName string) {
	if err := conf.IsValidPathName(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	if !s.doAuth(ctx, pathName) {
		return
	}

	pathConf, err := s.safeFindPathConf(pathName)
	if err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	if pathConf.OnvifPath != "" {
		pathConf, err = s.safeFindPathConf(pathConf.OnvifPath)
		if err != nil {
			s.writeError(ctx, http.StatusBadRequest, err)
			return
		}
	}

	q := eventstore.Query{Class: ctx.Query("class"), Desc: eventSortDesc(ctx.Query("sort"))}
	if v, ok := parseUnixQuery(ctx.Query("opened_at_gte")); ok {
		q.From = time.Unix(v, 0)
	}
	if v, ok := parseUnixQuery(ctx.Query("opened_at_lt")); ok {
		q.To = time.Unix(v, 0)
	}
	if raw := ctx.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			q.Limit = n
		}
	}

	spans, err := eventstore.QuerySpans(eventstore.LayoutFor(pathConf), q)
	if err != nil {
		s.writeError(ctx, http.StatusInternalServerError, err)
		return
	}
	out := make([]eventSpanJSON, 0, len(spans))
	for _, sp := range spans {
		dur := int64(sp.To.Sub(sp.From) / time.Second)
		if !sp.Point && dur < 1 && sp.To.After(sp.From) {
			dur = 1
		}
		if sp.Point {
			dur = 0
		}
		out = append(out, eventSpanJSON{
			From:     sp.From.Unix(),
			Duration: dur,
			Class:    sp.Class,
			Topic:    sp.Topic,
			Source:   sp.Source,
		})
	}
	ctx.Header("Cache-Control", "no-cache")
	ctx.JSON(http.StatusOK, gin.H{"events": out})
}
