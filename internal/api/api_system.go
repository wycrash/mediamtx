package api //nolint:revive

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/upgrade"
)

func (a *API) scheduleRestart() {
	go a.Parent.APIRestart()
}

func (a *API) onSystemRestart(ctx *gin.Context) {
	a.writeOK(ctx)
	if f, ok := ctx.Writer.(http.Flusher); ok {
		f.Flush()
	}
	a.scheduleRestart()
}

func (a *API) writeUpgradeError(ctx *gin.Context, err error) {
	status := http.StatusInternalServerError
	var unofficial *upgrade.ErrUnofficial
	if errors.As(err, &unofficial) {
		status = http.StatusBadRequest
	}
	a.writeError(ctx, status, err)
}

func (a *API) onSystemUpgradeGet(ctx *gin.Context) {
	out, err := a.Parent.APIUpgradeCheck()
	if err != nil {
		a.writeUpgradeError(ctx, err)
		return
	}
	ctx.JSON(http.StatusOK, out)
}

func (a *API) onSystemUpgradePost(ctx *gin.Context) {
	out, err := a.Parent.APIUpgrade()
	if err != nil {
		a.writeUpgradeError(ctx, err)
		return
	}

	ctx.JSON(http.StatusOK, out)
	if f, ok := ctx.Writer.(http.Flusher); ok {
		f.Flush()
	}

	if out.Upgraded {
		a.scheduleRestart()
	}
}
