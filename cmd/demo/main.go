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
	h := demo.NewHandlers(cfg, demo.NewMockAdmin(cfg), demo.NewEvolt(cfg), kafka)
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
