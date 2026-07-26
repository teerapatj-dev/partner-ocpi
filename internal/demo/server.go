package demo

import (
	"embed"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/rs/zerolog/log"
)

//go:embed ui
var uiFS embed.FS

func NewServer(cfg Config, h *Handlers) (*echo.Echo, error) {
	e := echo.New()
	e.HideBanner = true
	e.Use(echomw.Recover())
	// Global cap matches the mock's own limit — Evolt PUTs whole location
	// objects through /ocpi/*, and those can far exceed a form-sized body.
	e.Use(echomw.BodyLimit("2M"))

	ui, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return nil, err
	}
	e.StaticFS("/", ui)

	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	// Reads and writes get separate buckets: the UI polls state/requests every
	// few seconds, writes are human button presses.
	getLimiter := NewRateLimiter(cfg.RateGetPer10s)
	postLimiter := NewRateLimiter(cfg.RatePostPer10s)
	api := e.Group("/api/demo", echomw.BodyLimit("64K"), func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method == http.MethodGet {
				return getLimiter.Middleware()(next)(c)
			}
			return postLimiter.Middleware()(next)(c)
		}
	})

	api.GET("/state", h.State)
	api.GET("/requests", h.Requests)
	api.GET("/own/:kind", h.Own)
	api.GET("/received/:kind", h.Received)
	api.POST("/handshake/partner-init", h.PartnerInitHandshake)
	api.POST("/handshake/evolt-init", h.EvoltInitHandshake)
	api.POST("/unregister", h.Unregister)
	api.POST("/requests/clear", h.ClearRequests)
	api.POST("/push/location", h.PushLocation)
	api.POST("/push/evse-status", h.PushEvseStatus)
	api.POST("/push/tariff", h.PushTariff)
	api.DELETE("/push/tariff/:tariff_id", h.DeleteTariffPush)
	api.POST("/pull/:kind", h.PartnerPull)
	api.GET("/evolt/mirror", h.EvoltMirror)
	api.POST("/evolt/seed", h.InitEvolt)
	api.GET("/tables", h.Tables)
	api.GET("/station-evses", h.StationEvses)
	api.GET("/table/:table", h.BrowseTable)
	api.POST("/evolt/tariff-push", h.EvoltTariffPush)
	api.POST("/evolt/tariff-backfill", h.EvoltTariffBackfill)
	api.POST("/evolt/batch/:job", h.EvoltBatchRun)
	api.POST("/evolt/evse-status-event", h.EvoltEvseStatusEvent)
	api.POST("/reset", h.Reset)

	proxy, err := ocpiProxy(cfg.MockBaseURL)
	if err != nil {
		return nil, err
	}
	e.Any("/ocpi/*", proxy)

	return e, nil
}

// ocpiProxy forwards Evolt's OCPI callbacks verbatim to the mock, so a single
// public hostname serves the UI, the demo API and the partner's OCPI surface.
// Nothing about the request is logged here beyond method/path/status — the
// Authorization header passes through untouched and unrecorded.
func ocpiProxy(mockBaseURL string) (echo.HandlerFunc, error) {
	target, err := url.Parse(mockBaseURL)
	if err != nil {
		return nil, err
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	director := rp.Director
	rp.Director = func(req *http.Request) {
		director(req)
		req.Host = target.Host
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Warn().Str("method", r.Method).Str("path", r.URL.Path).Msg("ocpi proxy: mock unreachable")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"status_code":3000,"status_message":"partner mock unreachable"}`))
	}
	inner := echo.WrapHandler(rp)
	return func(c echo.Context) error {
		// The mock serves /admin on the same host:port; keeping dot segments
		// out means no normalizing hop added in front can ever fold a crafted
		// /ocpi/../admin path onto that surface.
		if strings.Contains(c.Request().URL.Path, "..") {
			return c.NoContent(http.StatusBadRequest)
		}
		return inner(c)
	}, nil
}
