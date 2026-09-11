package api //nolint:revive,dupl

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/compatapi"
	"github.com/bluenviron/mediamtx/internal/defs"
)

func (a *API) compatServerOrAbort(ctx *gin.Context) defs.APICompatServer {
	a.mutex.RLock()
	cs := a.CompatServer
	a.mutex.RUnlock()
	if interfaceIsEmpty(cs) {
		a.writeErrorNoLog(ctx, http.StatusServiceUnavailable, fmt.Errorf("compat API is not ready"))
		return nil
	}
	return cs
}

func (a *API) onCompatSessionsList(ctx *gin.Context) {
	cs := a.compatServerOrAbort(ctx)
	if cs == nil {
		return
	}
	data, err := cs.APISessionsList()
	if err != nil {
		a.writeError(ctx, http.StatusInternalServerError, err)
		return
	}

	data.ItemCount = len(data.Items)
	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		a.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}

func (a *API) onCompatSessionsGet(ctx *gin.Context) {
	cs := a.compatServerOrAbort(ctx)
	if cs == nil {
		return
	}
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		a.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	data, err := cs.APISessionsGet(id)
	if err != nil {
		if errors.Is(err, compatapi.ErrSessionNotFound) {
			a.writeError(ctx, http.StatusNotFound, err)
		} else {
			a.writeError(ctx, http.StatusInternalServerError, err)
		}
		return
	}

	ctx.JSON(http.StatusOK, data)
}

func (a *API) onCompatSessionsKick(ctx *gin.Context) {
	cs := a.compatServerOrAbort(ctx)
	if cs == nil {
		return
	}
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		a.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	err = cs.APISessionsKick(id)
	if err != nil {
		if errors.Is(err, compatapi.ErrSessionNotFound) {
			a.writeError(ctx, http.StatusNotFound, err)
		} else {
			a.writeError(ctx, http.StatusInternalServerError, err)
		}
		return
	}

	a.writeOK(ctx)
}

func (a *API) onCompatIndexRebuildAll(ctx *gin.Context) {
	cs := a.compatServerOrAbort(ctx)
	if cs == nil {
		return
	}
	data, err := cs.APIIndexRebuild("")
	if err != nil {
		a.writeError(ctx, http.StatusInternalServerError, err)
		return
	}
	ctx.JSON(http.StatusOK, data)
}

func (a *API) onCompatIndexRebuildPath(ctx *gin.Context) {
	cs := a.compatServerOrAbort(ctx)
	if cs == nil {
		return
	}
	pathName, ok := paramName(ctx)
	if !ok {
		a.writeError(ctx, http.StatusBadRequest, fmt.Errorf("invalid name"))
		return
	}

	data, err := cs.APIIndexRebuild(pathName)
	if err != nil {
		if errors.Is(err, compatapi.ErrPathNotFound) {
			a.writeError(ctx, http.StatusNotFound, err)
		} else {
			a.writeError(ctx, http.StatusBadRequest, err)
		}
		return
	}
	ctx.JSON(http.StatusOK, data)
}

func (a *API) onCompatIndexStatus(ctx *gin.Context) {
	cs := a.compatServerOrAbort(ctx)
	if cs == nil {
		return
	}
	data, err := cs.APIIndexStatus()
	if err != nil {
		a.writeError(ctx, http.StatusInternalServerError, err)
		return
	}
	ctx.JSON(http.StatusOK, data)
}
