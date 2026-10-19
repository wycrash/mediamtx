package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/onvif"
)

type onvifController interface {
	Enabled() bool
	DiscoverStart() (onvif.DiscoveryResult, error)
	DiscoverStop() onvif.DiscoveryResult
	DiscoverResult() onvif.DiscoveryResult
	Probe(ctx context.Context, address, user, pass string) (onvif.ProbeResult, error)
	Devices() []onvif.DeviceStatus
	Status() onvif.Status
}

func (a *API) onvifSvc() (onvifController, bool) {
	a.mutex.RLock()
	svc := a.Onvif
	a.mutex.RUnlock()
	if svc == nil || !svc.Enabled() {
		return nil, false
	}
	return svc, true
}

func (a *API) onOnvifDiscoverStart(ctx *gin.Context) {
	svc, ok := a.onvifSvc()
	if !ok {
		a.writeError(ctx, http.StatusConflict, fmt.Errorf("onvif is disabled"))
		return
	}
	res, err := svc.DiscoverStart()
	if err != nil {
		a.writeError(ctx, http.StatusConflict, err)
		return
	}
	ctx.JSON(http.StatusOK, res)
}

func (a *API) onOnvifDiscoverStop(ctx *gin.Context) {
	svc, ok := a.onvifSvc()
	if !ok {
		a.writeError(ctx, http.StatusConflict, fmt.Errorf("onvif is disabled"))
		return
	}
	ctx.JSON(http.StatusOK, svc.DiscoverStop())
}

func (a *API) onOnvifDiscoverResult(ctx *gin.Context) {
	svc, ok := a.onvifSvc()
	if !ok {
		a.writeError(ctx, http.StatusConflict, fmt.Errorf("onvif is disabled"))
		return
	}
	ctx.JSON(http.StatusOK, svc.DiscoverResult())
}

func (a *API) onOnvifProbe(ctx *gin.Context) {
	svc, ok := a.onvifSvc()
	if !ok {
		a.writeError(ctx, http.StatusConflict, fmt.Errorf("onvif is disabled"))
		return
	}
	var req struct {
		Address string `json:"address"`
		User    string `json:"user"`
		Pass    string `json:"pass"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		a.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx.Request.Context(), 30*time.Second)
	defer cancel()
	res, err := svc.Probe(cctx, req.Address, req.User, req.Pass)
	if err != nil {
		a.writeError(ctx, http.StatusBadGateway, err)
		return
	}
	ctx.JSON(http.StatusOK, res)
}

func (a *API) onOnvifPathsAdd(ctx *gin.Context) {
	svc, ok := a.onvifSvc()
	if !ok {
		a.writeError(ctx, http.StatusConflict, fmt.Errorf("onvif is disabled"))
		return
	}
	var req struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		User    string `json:"user"`
		Pass    string `json:"pass"`
		Streams []struct {
			Profile string `json:"profile"`
			Name    string `json:"name"`
			Record  *bool  `json:"record"`
			Events  *bool  `json:"events"`
		} `json:"streams"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		a.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	cctx, cancel := context.WithTimeout(ctx.Request.Context(), 30*time.Second)
	defer cancel()
	probe, err := svc.Probe(cctx, req.Address, req.User, req.Pass)
	if err != nil {
		a.writeError(ctx, http.StatusBadGateway, err)
		return
	}

	snap := a.Parent.APIConfigSnapshot()
	owner, err := onvif.OwnerName(snap.Paths, probe.Endpoint)
	if err != nil {
		a.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	add := onvif.AddRequest{
		Name:    req.Name,
		Address: req.Address,
		User:    req.User,
		Pass:    req.Pass,
	}
	for _, st := range req.Streams {
		add.Streams = append(add.Streams, onvif.StreamRequest{
			Profile: st.Profile,
			Name:    st.Name,
			Record:  st.Record,
			Events:  st.Events,
		})
	}
	planned, err := onvif.PlanAdd(add, probe, snap.Paths, owner)
	if err != nil {
		a.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	var created []string
	for _, path := range planned {
		optional, err := optionalFromPlanned(path)
		if err != nil {
			a.rollbackOnvifPaths(created)
			a.writeError(ctx, http.StatusBadRequest, err)
			return
		}
		if err := a.Parent.APIConfigPathsAdd(path.Name, optional); err != nil {
			a.rollbackOnvifPaths(created)
			a.writeError(ctx, http.StatusBadRequest, err)
			return
		}
		created = append(created, path.Name)
	}
	ctx.JSON(http.StatusOK, gin.H{"paths": created})
}

func (a *API) rollbackOnvifPaths(names []string) {
	for i := len(names) - 1; i >= 0; i-- {
		_ = a.Parent.APIConfigPathsDelete(names[i])
	}
}

func optionalFromPlanned(path onvif.PlannedPath) (conf.OptionalPath, error) {
	body := map[string]any{
		"source": path.Source,
		"record": path.Record,
	}
	if path.OnvifAddress != "" {
		body["onvifAddress"] = path.OnvifAddress
		body["onvifUser"] = path.OnvifUser
		body["onvifPass"] = path.OnvifPass
		body["onvifEvents"] = path.OnvifEvents
	}
	if path.OnvifPath != "" {
		body["onvifPath"] = path.OnvifPath
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return conf.OptionalPath{}, err
	}
	var optional conf.OptionalPath
	if err := json.Unmarshal(raw, &optional); err != nil {
		return conf.OptionalPath{}, err
	}
	return optional, nil
}

func (a *API) onOnvifDevices(ctx *gin.Context) {
	svc, ok := a.onvifSvc()
	if !ok {
		a.writeError(ctx, http.StatusConflict, fmt.Errorf("onvif is disabled"))
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"devices": svc.Devices()})
}

func (a *API) onOnvifStatus(ctx *gin.Context) {
	svc, ok := a.onvifSvc()
	if !ok {
		a.writeError(ctx, http.StatusConflict, fmt.Errorf("onvif is disabled"))
		return
	}
	ctx.JSON(http.StatusOK, svc.Status())
}
