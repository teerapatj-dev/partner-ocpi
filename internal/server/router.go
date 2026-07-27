package server

import (
	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"

	"mock-ocpi-partner/internal/client"
	"mock-ocpi-partner/internal/config"
	"mock-ocpi-partner/internal/handlers"
	"mock-ocpi-partner/internal/middleware"
	"mock-ocpi-partner/internal/store"
)

func New(cfg config.Config, st *store.Store, cl *client.Client) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.Use(echomw.Recover())
	e.Use(echomw.BodyLimit("2M"))
	e.Use(echomw.CORSWithConfig(echomw.CORSConfig{
		AllowOrigins: cfg.CORSOrigins,
		AllowHeaders: []string{echo.HeaderContentType, echo.HeaderAuthorization, "X-API-Key"},
		AllowMethods: []string{echo.GET, echo.POST, echo.PUT, echo.PATCH, echo.DELETE, echo.OPTIONS},
	}))

	h := handlers.New(cfg, st, cl)
	e.GET("/health", h.Health)

	ocpiGroup := e.Group("/ocpi", middleware.RequestLog(st))

	handshake := ocpiGroup.Group("", middleware.OCPIAuth(st, middleware.ScopeHandshake))
	handshake.GET("/versions", h.Versions)
	handshake.GET("/2.2.1", h.VersionDetails)
	handshake.POST("/2.2.1/credentials", h.PostCredentials)
	handshake.GET("/2.2.1/credentials", h.GetCredentials)
	handshake.PUT("/2.2.1/credentials", h.PutCredentials)
	handshake.DELETE("/2.2.1/credentials", h.DeleteCredentials)

	registered := ocpiGroup.Group("", middleware.OCPIAuth(st, middleware.ScopeRegistered))

	cpo := registered.Group("/cpo/2.2.1")
	cpo.GET("/locations", h.ListLocations)
	cpo.GET("/locations/:location_id", h.GetLocationObject)
	cpo.GET("/locations/:location_id/:evse_uid", h.GetLocationObject)
	cpo.GET("/locations/:location_id/:evse_uid/:connector_id", h.GetLocationObject)
	cpo.GET("/tariffs", h.ListTariffs)
	cpo.GET("/sessions", h.StubEmptyList)
	cpo.GET("/cdrs", h.StubEmptyList)
	cpo.POST("/commands/:command", h.StubCommand)

	emsp := registered.Group("/emsp/2.2.1")
	for _, path := range []string{
		"/locations/:country_code/:party_id/:location_id",
		"/locations/:country_code/:party_id/:location_id/:evse_uid",
		"/locations/:country_code/:party_id/:location_id/:evse_uid/:connector_id",
	} {
		emsp.PUT(path, h.PutLocationObject)
		emsp.PATCH(path, h.PatchLocationObject)
		emsp.GET(path, h.GetReceivedLocationObject)
	}
	emsp.PUT("/tariffs/:country_code/:party_id/:tariff_id", h.PutReceivedTariff)
	emsp.PATCH("/tariffs/:country_code/:party_id/:tariff_id", h.PatchTariffNotAllowed)
	emsp.DELETE("/tariffs/:country_code/:party_id/:tariff_id", h.DeleteReceivedTariff)
	emsp.GET("/tariffs/:country_code/:party_id/:tariff_id", h.GetReceivedTariff)
	for _, module := range []string{"sessions", "cdrs"} {
		emsp.PUT("/"+module+"/:country_code/:party_id/:object_id", h.StubAccept)
		emsp.PATCH("/"+module+"/:country_code/:party_id/:object_id", h.StubAccept)
		emsp.GET("/"+module+"/:country_code/:party_id/:object_id", h.StubAccept)
	}

	admin := e.Group("/admin", middleware.APIKeyAuth(cfg.AdminAPIKey))
	admin.POST("/tokens", h.AdminCreateToken)
	admin.POST("/handshake", h.AdminHandshake)
	admin.GET("/tokens/current", h.AdminCurrentTokens)
	admin.POST("/push/location", h.AdminPushLocation)
	admin.POST("/push/evse-status", h.AdminPushEvseStatus)
	admin.POST("/push/tariff", h.AdminPushTariff)
	admin.DELETE("/push/tariff/:tariff_id", h.AdminDeleteTariffPush)
	admin.POST("/pull/:kind", h.AdminPullCounterparty)
	admin.GET("/registrations", h.AdminRegistrations)
	admin.DELETE("/registrations", h.AdminUnregister)
	admin.GET("/state", h.AdminState)
	admin.GET("/own/:kind", h.AdminOwn)
	admin.GET("/received/:kind", h.AdminReceived)
	admin.POST("/received/locations", h.AdminInjectLocation)
	admin.GET("/requests", h.AdminRequests)
	admin.DELETE("/requests", h.AdminClearRequests)
	admin.POST("/seed/reset", h.AdminSeedReset)
	admin.POST("/seed/new-batch", h.AdminNewBatch)

	return e
}
