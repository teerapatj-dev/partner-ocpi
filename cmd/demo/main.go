package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"mock-ocpi-partner/internal/demo"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	cfg, err := demo.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("invalid configuration")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	kafka := demo.NewKafka(cfg)
	if enabled, reason := kafka.Enabled(); !enabled {
		log.Warn().Str("reason", reason).Msg("kafka demo disabled")
	}

	var charger *demo.ChargerDB
	if cfg.EvoltDBDSN != "" && cfg.KafkaEvseID != "" {
		charger, err = demo.NewChargerDB(ctx, cfg.EvoltDBDSN, cfg.KafkaEvseID)
		if err != nil {
			log.Warn().Err(err).Msg("charger simulator off — cannot reach Evolt DB")
		} else {
			defer charger.Close()
			log.Info().Msg("charger simulator on — evse-status will show real values")
		}
	}

	var browser *demo.DBBrowser
	if cfg.EvoltDBDSN != "" || cfg.PartnerDBDSN != "" {
		browser, err = demo.NewDBBrowser(ctx, cfg.EvoltDBDSN, cfg.PartnerDBDSN, cfg.KafkaStationID)
		if err != nil {
			log.Warn().Err(err).Msg("table browser off — cannot reach a configured DB")
			browser = nil
		} else {
			defer browser.Close()
			log.Info().Msg("table browser on — Tables menu + Evolt seed available")
		}
	}

	h := demo.NewHandlers(cfg, demo.NewMockAdmin(cfg), demo.NewEvolt(cfg), kafka, charger, browser)
	e, err := demo.NewServer(cfg, h)
	if err != nil {
		log.Fatal().Err(err).Msg("build server")
	}

	go func() {
		log.Info().
			Str("public_base_url", cfg.PublicBaseURL).
			Str("port", cfg.Port).
			Msg("ocpi web demo listening")
		if err := e.Start(":" + cfg.Port); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("server stopped")
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		log.Warn().Err(err).Msg("graceful shutdown failed")
	}
}
