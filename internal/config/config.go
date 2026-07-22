package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	PartnerName     string
	PartyID         string
	CountryCode     string
	Currency        string
	Port            string
	BaseURL         string
	DatabaseURL     string
	AdminAPIKey     string
	CORSOrigins     []string
	StrictCallback  bool
	OutboundTimeout time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		PartnerName:     os.Getenv("PARTNER_NAME"),
		PartyID:         strings.ToUpper(os.Getenv("PARTNER_PARTY_ID")),
		CountryCode:     strings.ToUpper(getenv("PARTNER_COUNTRY_CODE", "TH")),
		Currency:        strings.ToUpper(getenv("PARTNER_CURRENCY", "THB")),
		Port:            getenv("PORT", getenv("HTTP_PORT", "8080")),
		BaseURL:         strings.TrimRight(getenv("BASE_URL", os.Getenv("RENDER_EXTERNAL_URL")), "/"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		AdminAPIKey:     os.Getenv("ADMIN_API_KEY"),
		StrictCallback:  boolenv("STRICT_CALLBACK"),
		OutboundTimeout: 10 * time.Second,
	}
	for _, o := range strings.Split(getenv("CORS_ORIGINS", "*"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.CORSOrigins = append(cfg.CORSOrigins, o)
		}
	}
	if s := os.Getenv("OUTBOUND_TIMEOUT_SECONDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			cfg.OutboundTimeout = time.Duration(n) * time.Second
		}
	}
	if len(cfg.PartyID) != 3 {
		return cfg, fmt.Errorf("PARTNER_PARTY_ID must be exactly 3 chars, got %q", cfg.PartyID)
	}
	if len(cfg.CountryCode) != 2 {
		return cfg, fmt.Errorf("PARTNER_COUNTRY_CODE must be exactly 2 chars, got %q", cfg.CountryCode)
	}
	if cfg.PartnerName == "" {
		return cfg, fmt.Errorf("PARTNER_NAME is required")
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:" + cfg.Port
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func boolenv(key string) bool {
	v, _ := strconv.ParseBool(os.Getenv(key))
	return v
}
