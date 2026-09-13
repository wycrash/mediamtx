// Package api contains the API server.
package api //nolint:revive

import (
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/httpp"
	"github.com/bluenviron/mediamtx/internal/webui"
)

const (
	maxInboundConfigSize = 10 * 1024 * 1024
)

func interfaceIsEmpty(i any) bool {
	return reflect.ValueOf(i).Kind() != reflect.Pointer || reflect.ValueOf(i).IsNil()
}

func sortedKeys(paths map[string]*conf.Path) []string {
	ret := make([]string, len(paths))
	i := 0
	for name := range paths {
		ret[i] = name
		i++
	}
	sort.Strings(ret)
	return ret
}

func paramName(ctx *gin.Context) (string, bool) {
	name := ctx.Param("name")

	if len(name) < 2 || name[0] != '/' {
		return "", false
	}

	return name[1:], true
}

type apiAuthManager interface {
	Authenticate(req *auth.Request) (string, *auth.Error)
	RefreshJWTJWKS()
}

type apiParent interface {
	logger.Writer
	APIConfigSnapshot() *conf.Conf
	APIConfigGlobalPatch(conf.OptionalGlobal) error
	APIConfigPathDefaultsPatch(conf.OptionalPath) error
	APIConfigPathsAdd(string, conf.OptionalPath) error
	APIConfigPathsPatch(string, conf.OptionalPath) error
	APIConfigPathsReplace(string, conf.OptionalPath) error
	APIConfigPathsDelete(string) error
	APIRestart()
	APIUpgradeCheck() (*defs.APIUpgrade, error)
	APIUpgrade() (*defs.APIUpgrade, error)
}

type systemMetricsProvider interface {
	Snapshot() defs.APISystemMetrics
}

// API is an API server.
type API struct {
	Version        string
	Started        time.Time
	Address        string
	DumpPackets    bool
	Encryption     bool
	ServerKey      string
	ServerCert     string
	AllowOrigins   []string
	TrustedProxies conf.IPNetworks
	ReadTimeout    conf.Duration
	WriteTimeout   conf.Duration
	AuthManager    apiAuthManager
	PathManager    defs.APIPathManager
	RTSPServer     defs.APIRTSPServer
	RTSPSServer    defs.APIRTSPServer
	RTMPServer     defs.APIRTMPServer
	RTMPSServer    defs.APIRTMPServer
	HLSServer      defs.APIHLSServer
	CompatServer   defs.APICompatServer
	WebRTCServer   defs.APIWebRTCServer
	SRTServer      defs.APISRTServer
	MoQServer      defs.APIMoQServer
	SystemMetrics  systemMetricsProvider
	Logs           *logger.Ring
	Parent         apiParent
	Admin          fs.FS

	httpServer *httpp.Server
	mutex      sync.RWMutex
}

// Initialize initializes API.
func (a *API) Initialize() error {
	router := gin.New()
	router.SetTrustedProxies(a.TrustedProxies.ToTrustedProxies()) //nolint:errcheck
	router.Use(a.middlewarePreflightRequests)
	router.Use(a.middlewareAuth)

	group := router.Group("/v3")

	group.GET("/info", a.onInfo)
	group.GET("/logs/list", a.onLogsList)
	group.GET("/metrics/system", a.onSystemMetrics)
	group.POST("/system/restart", a.onSystemRestart)
	group.GET("/system/upgrade", a.onSystemUpgradeGet)
	group.POST("/system/upgrade", a.onSystemUpgradePost)

	group.POST("/auth/jwks/refresh", a.onAuthJwksRefresh)

	group.GET("/config/global/get", a.onConfigGlobalGet)
	group.PATCH("/config/global/patch", a.onConfigGlobalPatch)

	group.GET("/config/path-defaults/get", a.onConfigPathDefaultsGet)
	group.PATCH("/config/path-defaults/patch", a.onConfigPathDefaultsPatch)

	// deprecated
	group.GET("/config/pathdefaults/get", a.onConfigPathDefaultsGet)
	group.PATCH("/config/pathdefaults/patch", a.onConfigPathDefaultsPatch)

	group.GET("/config/paths/list", a.onConfigPathsList)
	group.GET("/config/paths/get/*name", a.onConfigPathsGet)
	group.POST("/config/paths/add/*name", a.onConfigPathsAdd)
	group.PATCH("/config/paths/patch/*name", a.onConfigPathsPatch)
	group.POST("/config/paths/replace/*name", a.onConfigPathsReplace)
	group.DELETE("/config/paths/delete/*name", a.onConfigPathsDelete)

	group.GET("/paths/list", a.onPathsList)
	group.GET("/paths/get/*name", a.onPathsGet)
	group.GET("/paths/forward-dests/list", a.onForwardDestsList)
	group.GET("/paths/forward-dests/get", a.onForwardDestsGet)
	group.GET("/paths/static-sources/get/*name", a.onStaticSourcesGet)

	// deprecated
	group.GET("/paths/forward/list", a.onForwardDestsList)
	group.GET("/paths/forward/get", a.onForwardDestsGet)

	if !interfaceIsEmpty(a.HLSServer) {
		group.GET("/hls/muxers/list", a.onHLSMuxersList)
		group.GET("/hls/muxers/get/*name", a.onHLSMuxersGet)
		group.GET("/hls/sessions/list", a.onHLSSessionsList)
		group.GET("/hls/sessions/get/:id", a.onHLSSessionsGet)
		group.POST("/hls/sessions/kick/:id", a.onHLSSessionsKick)

		// deprecated
		group.GET("/hlsmuxers/list", a.onHLSMuxersList)
		group.GET("/hlsmuxers/get/*name", a.onHLSMuxersGet)
		group.GET("/hlssessions/list", a.onHLSSessionsList)
		group.GET("/hlssessions/get/:id", a.onHLSSessionsGet)
		group.POST("/hlssessions/kick/:id", a.onHLSSessionsKick)
	}

	group.GET("/compatsessions/list", a.onCompatSessionsList)
	group.GET("/compatsessions/get/:id", a.onCompatSessionsGet)
	group.POST("/compatsessions/kick/:id", a.onCompatSessionsKick)
	group.GET("/compatindex/status", a.onCompatIndexStatus)
	group.POST("/compatindex/rebuild", a.onCompatIndexRebuildAll)
	group.POST("/compatindex/rebuild/*name", a.onCompatIndexRebuildPath)

	if !interfaceIsEmpty(a.RTSPServer) {
		group.GET("/rtsp/conns/list", a.onRTSPConnsList)
		group.GET("/rtsp/conns/get/:id", a.onRTSPConnsGet)
		group.GET("/rtsp/sessions/list", a.onRTSPSessionsList)
		group.GET("/rtsp/sessions/get/:id", a.onRTSPSessionsGet)
		group.POST("/rtsp/sessions/kick/:id", a.onRTSPSessionsKick)

		// deprecated
		group.GET("/rtspconns/list", a.onRTSPConnsList)
		group.GET("/rtspconns/get/:id", a.onRTSPConnsGet)
		group.GET("/rtspsessions/list", a.onRTSPSessionsList)
		group.GET("/rtspsessions/get/:id", a.onRTSPSessionsGet)
		group.POST("/rtspsessions/kick/:id", a.onRTSPSessionsKick)
	}

	if !interfaceIsEmpty(a.RTSPSServer) {
		group.GET("/rtsps/conns/list", a.onRTSPSConnsList)
		group.GET("/rtsps/conns/get/:id", a.onRTSPSConnsGet)
		group.GET("/rtsps/sessions/list", a.onRTSPSSessionsList)
		group.GET("/rtsps/sessions/get/:id", a.onRTSPSSessionsGet)
		group.POST("/rtsps/sessions/kick/:id", a.onRTSPSSessionsKick)

		// deprecated
		group.GET("/rtspsconns/list", a.onRTSPSConnsList)
		group.GET("/rtspsconns/get/:id", a.onRTSPSConnsGet)
		group.GET("/rtspssessions/list", a.onRTSPSSessionsList)
		group.GET("/rtspssessions/get/:id", a.onRTSPSSessionsGet)
		group.POST("/rtspssessions/kick/:id", a.onRTSPSSessionsKick)
	}

	if !interfaceIsEmpty(a.RTMPServer) {
		group.GET("/rtmp/conns/list", a.onRTMPConnsList)
		group.GET("/rtmp/conns/get/:id", a.onRTMPConnsGet)
		group.POST("/rtmp/conns/kick/:id", a.onRTMPConnsKick)

		// deprecated
		group.GET("/rtmpconns/list", a.onRTMPConnsList)
		group.GET("/rtmpconns/get/:id", a.onRTMPConnsGet)
		group.POST("/rtmpconns/kick/:id", a.onRTMPConnsKick)
	}

	if !interfaceIsEmpty(a.RTMPSServer) {
		group.GET("/rtmps/conns/list", a.onRTMPSConnsList)
		group.GET("/rtmps/conns/get/:id", a.onRTMPSConnsGet)
		group.POST("/rtmps/conns/kick/:id", a.onRTMPSConnsKick)

		// deprecated
		group.GET("/rtmpsconns/list", a.onRTMPSConnsList)
		group.GET("/rtmpsconns/get/:id", a.onRTMPSConnsGet)
		group.POST("/rtmpsconns/kick/:id", a.onRTMPSConnsKick)
	}

	if !interfaceIsEmpty(a.WebRTCServer) {
		group.GET("/webrtc/sessions/list", a.onWebRTCSessionsList)
		group.GET("/webrtc/sessions/get/:id", a.onWebRTCSessionsGet)
		group.POST("/webrtc/sessions/kick/:id", a.onWebRTCSessionsKick)

		// deprecated
		group.GET("/webrtcsessions/list", a.onWebRTCSessionsList)
		group.GET("/webrtcsessions/get/:id", a.onWebRTCSessionsGet)
		group.POST("/webrtcsessions/kick/:id", a.onWebRTCSessionsKick)
	}

	if !interfaceIsEmpty(a.SRTServer) {
		group.GET("/srt/conns/list", a.onSRTConnsList)
		group.GET("/srt/conns/get/:id", a.onSRTConnsGet)
		group.POST("/srt/conns/kick/:id", a.onSRTConnsKick)

		// deprecated
		group.GET("/srtconns/list", a.onSRTConnsList)
		group.GET("/srtconns/get/:id", a.onSRTConnsGet)
		group.POST("/srtconns/kick/:id", a.onSRTConnsKick)
	}

	if !interfaceIsEmpty(a.MoQServer) {
		group.GET("/moq/sessions/list", a.onMoQSessionsList)
		group.GET("/moq/sessions/get/:id", a.onMoQSessionsGet)
		group.POST("/moq/sessions/kick/:id", a.onMoQSessionsKick)

		// deprecated
		group.GET("/moqsessions/list", a.onMoQSessionsList)
		group.GET("/moqsessions/get/:id", a.onMoQSessionsGet)
		group.POST("/moqsessions/kick/:id", a.onMoQSessionsKick)
	}

	group.GET("/recordings/list", a.onRecordingsList)
	group.GET("/recordings/get/*name", a.onRecordingsGet)
	group.DELETE("/recordings/segments/delete", a.onRecordingsSegmentsDelete)

	// deprecated
	group.DELETE("/recordings/deletesegment", a.onRecordingsSegmentsDelete)

	router.NoRoute(a.onUI)

	a.httpServer = &httpp.Server{
		Address:           a.Address,
		AllowOrigins:      a.AllowOrigins,
		DumpPackets:       a.DumpPackets,
		DumpPacketsPrefix: "api_server_conn",
		ReadTimeout:       time.Duration(a.ReadTimeout),
		WriteTimeout:      time.Duration(a.WriteTimeout),
		Encryption:        a.Encryption,
		ServerCert:        a.ServerCert,
		ServerKey:         a.ServerKey,
		Handler:           router,
		Parent:            a,
	}
	err := a.httpServer.Initialize()
	if err != nil {
		return err
	}

	str := "started with listener on " + a.Address
	if !a.Encryption {
		str += " (TCP/HTTP)"
	} else {
		str += " (TCP/HTTPS)"
	}
	a.Log(logger.Info, str)

	return nil
}

// Close closes the API.
func (a *API) Close() {
	a.Log(logger.Info, "closing")

	a.httpServer.Close()

	a.Log(logger.Debug, "closed")
}

// Log implements logger.Writer.
func (a *API) Log(level logger.Level, format string, args ...any) {
	a.Parent.Log(level, "[API] "+format, args...)
}

func (a *API) writePathNotFound(ctx *gin.Context, name string) {
	a.writeError(ctx, http.StatusNotFound, conf.PathNotFound(name))
}

func (a *API) writeError(ctx *gin.Context, status int, err error) {
	// show error in logs
	a.Log(logger.Error, err.Error())

	// add error to response
	ctx.AbortWithStatusJSON(status, &defs.APIError{
		Status: defs.APIErrorStatusError,
		Error:  err.Error(),
	})
}

func (a *API) writeErrorNoLog(ctx *gin.Context, status int, err error) {
	ctx.AbortWithStatusJSON(status, &defs.APIError{
		Status: defs.APIErrorStatusError,
		Error:  err.Error(),
	})
}

func (a *API) writeOK(ctx *gin.Context) {
	ctx.JSON(http.StatusOK, &defs.APIOK{Status: defs.APIOKStatusOK})
}

func (a *API) middlewarePreflightRequests(ctx *gin.Context) {
	if ctx.Request.Method == http.MethodOptions &&
		ctx.Request.Header.Get("Access-Control-Request-Method") != "" {
		ctx.Header("Access-Control-Allow-Methods", "OPTIONS, GET, POST, PATCH, DELETE")
		ctx.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		ctx.AbortWithStatus(http.StatusNoContent)
		return
	}
}

func skipAPIAuth(p string) bool {
	return p == "/" || p == "/admin" || strings.HasPrefix(p, "/admin/")
}

func (a *API) middlewareAuth(ctx *gin.Context) {
	if skipAPIAuth(ctx.Request.URL.Path) {
		return
	}

	req := &auth.Request{
		Action:               conf.AuthActionAPI,
		Query:                ctx.Request.URL.RawQuery,
		Credentials:          httpp.Credentials(ctx.Request),
		IP:                   net.ParseIP(ctx.ClientIP()),
		EnableAskCredentials: true,
	}

	_, err := a.AuthManager.Authenticate(req)
	if err != nil {
		if err.AskCredentials {
			ctx.Header("WWW-Authenticate", `Basic realm="mediamtx"`)
			a.writeErrorNoLog(ctx, http.StatusUnauthorized, fmt.Errorf("authentication error"))
			return
		}

		auth.LogAndDelayError(&logger.InlineWriter{
			Parent: a,
			Prefix: fmt.Sprintf("[conn %v]", httpp.RemoteAddr(ctx)),
		}, err)

		a.writeErrorNoLog(ctx, http.StatusUnauthorized, fmt.Errorf("authentication error"))
		return
	}
}

func (a *API) adminFS() fs.FS {
	if a.Admin != nil {
		return a.Admin
	}
	return webui.Admin()
}

func (a *API) onUI(ctx *gin.Context) {
	if ctx.Request.Method != http.MethodGet && ctx.Request.Method != http.MethodHead {
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}

	pa := ctx.Request.URL.Path
	switch {
	case pa == "/":
		ctx.Redirect(http.StatusFound, "/admin/")
	case pa == "/admin":
		ctx.Redirect(http.StatusFound, "/admin/")
	case strings.HasPrefix(pa, "/admin/"):
		webui.ServeSPA(ctx, a.adminFS(), strings.TrimPrefix(pa, "/admin/"))
	default:
		ctx.AbortWithStatus(http.StatusNotFound)
	}
}

func (a *API) onInfo(ctx *gin.Context) {
	ctx.JSON(http.StatusOK, &defs.APIInfo{
		Version: a.Version,
		Started: a.Started,
	})
}

func (a *API) onSystemMetrics(ctx *gin.Context) {
	if a.SystemMetrics == nil {
		ctx.JSON(http.StatusOK, &defs.APISystemMetrics{
			CollectedAt: time.Now(),
			Disks:       []defs.APISystemMetricsDisk{},
			Network:     []defs.APISystemMetricsNIC{},
			History:     []defs.APISystemMetricsPoint{},
		})
		return
	}

	snap := a.SystemMetrics.Snapshot()
	if snap.Disks == nil {
		snap.Disks = []defs.APISystemMetricsDisk{}
	}
	if snap.Network == nil {
		snap.Network = []defs.APISystemMetricsNIC{}
	}
	if snap.History == nil {
		snap.History = []defs.APISystemMetricsPoint{}
	}
	ctx.JSON(http.StatusOK, &snap)
}

func (a *API) onAuthJwksRefresh(ctx *gin.Context) {
	a.AuthManager.RefreshJWTJWKS()
	a.writeOK(ctx)
}

// SetCompatServer attaches or clears the Compat API after it starts or stops.
// The control API may already be listening while the DVR index is still loading.
func (a *API) SetCompatServer(cs defs.APICompatServer) {
	if a == nil {
		return
	}
	a.mutex.Lock()
	a.CompatServer = cs
	a.mutex.Unlock()
}
