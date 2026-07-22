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

	"mock-ocpi-partner/internal/client"
	"mock-ocpi-partner/internal/config"
	"mock-ocpi-partner/internal/seed"
	"mock-ocpi-partner/internal/server"
	"mock-ocpi-partner/internal/store"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("invalid configuration")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("open store")
	}
	defer st.Close()

	seeded, err := seed.EnsureSeeded(ctx, st, cfg.PartyID)
	if err != nil {
		log.Fatal().Err(err).Msg("seed")
	}
	if seeded > 0 {
		log.Info().Int("objects", seeded).Msg("seeded demo dataset")
	}

	cl := client.New(cfg, st)
	e := server.New(cfg, st, cl)

	go func() {
		log.Info().
			Str("partner", cfg.PartyID).
			Str("base_url", cfg.BaseURL).
			Str("port", cfg.Port).
			Msg("mock OCPI partner listening")
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
