package hooks

import (
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
)

// OnEventParams are the parameters of OnEvent.
type OnEventParams struct {
	Logger  logger.Writer
	Pool    *externalcmd.Pool
	Cmd     string
	Restart bool
	Env     externalcmd.Environment
}

// OnEvent runs the runOnEvent command for one stored edge.
func OnEvent(params OnEventParams) {
	if params.Cmd == "" || params.Pool == nil {
		return
	}
	if params.Logger != nil {
		params.Logger.Log(logger.Debug, "runOnEvent command launched")
	}
	cmd := &externalcmd.Cmd{
		Pool:    params.Pool,
		Cmdstr:  params.Cmd,
		Restart: params.Restart,
		Env:     params.Env,
	}
	cmd.Start()
}
