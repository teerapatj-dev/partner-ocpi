package demo

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config drives the demo orchestrator. Only the mock side is mandatory: every
// Evolt URL may be absent (dev cluster down, or local run against stubs) and
// the matching endpoints then fail with an explanation instead of at startup.
type Config struct {
	Port          string
	PublicBaseURL string
	MockBaseURL   string
	MockAdminKey  string

	EvoltVersionsURL string
	EvoltOrchURL     string
	EvoltAdapterURL  string
	EvoltRoamingURL  string
	OrchAPIKey       string
	RoamingAPIKey    string

	AllowedStations []string

	KafkaEnabled   bool
	KafkaBroker    string
	KafkaTopic     string
	KafkaPartition int
	KafkaStationID string
	KafkaEvseUID   string
	KafkaEvseID    string
	KafkaPartyCC   string
	KafkaPartyID   string

	RatePostPer10s int
	RateGetPer10s  int
	Timeout        time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Port:             getenv("DEMO_PORT", "8080"),
		PublicBaseURL:    strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"),
		MockBaseURL:      strings.TrimRight(os.Getenv("MOCK_BASE_URL"), "/"),
		MockAdminKey:     os.Getenv("MOCK_ADMIN_KEY"),
		EvoltVersionsURL: os.Getenv("EVOLT_VERSIONS_URL"),
		EvoltOrchURL:     strings.TrimRight(os.Getenv("EVOLT_ORCH_URL"), "/"),
		EvoltAdapterURL:  strings.TrimRight(os.Getenv("EVOLT_ADAPTER_URL"), "/"),
		EvoltRoamingURL:  strings.TrimRight(os.Getenv("EVOLT_ROAMING_URL"), "/"),
		OrchAPIKey:       os.Getenv("EVOLT_ORCH_API_KEY"),
		RoamingAPIKey:    os.Getenv("EVOLT_ROAMING_API_KEY"),
		KafkaEnabled:     boolenvDefault("DEMO_ENABLE_KAFKA", true),
		KafkaBroker:      getenv("DEMO_KAFKA_BROKER", "redpanda-0.np.th.evtech.dev:9094"),
		KafkaTopic:       getenv("DEMO_KAFKA_TOPIC", "dev.aurora.ocpi.event.evse_status"),
		KafkaPartition:   intenv("DEMO_KAFKA_PARTITION", 2),
		KafkaStationID:   os.Getenv("DEMO_KAFKA_STATION_ID"),
		KafkaEvseUID:     os.Getenv("DEMO_KAFKA_EVSE_UID"),
		KafkaEvseID:      os.Getenv("DEMO_KAFKA_EVSE_ID"),
		KafkaPartyCC:     getenv("DEMO_KAFKA_PARTY_CC", "TH"),
		KafkaPartyID:     getenv("DEMO_KAFKA_PARTY_ID", "EVO"),
		RatePostPer10s:   intenv("DEMO_RATE_POST_PER_10S", 10),
		RateGetPer10s:    intenv("DEMO_RATE_GET_PER_10S", 60),
		Timeout:          time.Duration(intenv("DEMO_TIMEOUT_SECONDS", 30)) * time.Second,
	}
	for _, s := range strings.Split(os.Getenv("DEMO_ALLOWED_STATION_IDS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			cfg.AllowedStations = append(cfg.AllowedStations, s)
		}
	}
	if cfg.MockBaseURL == "" {
		return cfg, fmt.Errorf("MOCK_BASE_URL is required")
	}
	if cfg.MockAdminKey == "" {
		return cfg, fmt.Errorf("MOCK_ADMIN_KEY is required")
	}
	if cfg.KafkaEnabled && (cfg.KafkaStationID == "" || cfg.KafkaEvseUID == "" || cfg.KafkaEvseID == "") {
		cfg.KafkaEnabled = false
	}
	return cfg, nil
}

func (c Config) StationAllowed(id string) bool {
	for _, s := range c.AllowedStations {
		if s == id {
			return true
		}
	}
	return false
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intenv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func boolenvDefault(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
