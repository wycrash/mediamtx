package api //nolint:revive

import (
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
)

func (a *API) onLogsList(ctx *gin.Context) {
	filter := logger.ListFilter{Path: ctx.Query("path")}

	var entries []logger.Entry
	if a.Logs != nil {
		entries = a.Logs.List(filter)
	}

	items := make([]defs.APILogEntry, len(entries))
	for i, e := range entries {
		items[i] = defs.APILogEntry{
			Timestamp: e.Time,
			Level:     conf.LogLevel(e.Level),
			Message:   e.Message,
		}
	}
	slices.Reverse(items)

	data := defs.APILogList{
		ItemCount: len(items),
		Items:     items,
	}

	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		a.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}
