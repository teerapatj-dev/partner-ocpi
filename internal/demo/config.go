package demo

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// FanoutMock is one extra partner instance the demo can register as a fanout target. PathPrefix is
// the public-URL segment its OCPI callbacks are proxied under (e.g. "/vct" → /vct/ocpi/*).
type FanoutMock struct {
	Key        string // short id used in routes and labels: "vct", "chx"
	URL        string // in-network base URL of that mock instance
	AdminKey   string
	PathPrefix string
	DBDSN      string // that instance's own Postgres — read-only table browser; "" = browser shows PLG only
}

// Config drives the demo orchestrator. Only the mock side is mandatory: every
// Evolt URL may be absent (dev cluster down, or local run against stubs) and
// the matching endpoints then fail with an explanation instead of at startup.
type Config struct {
	Port          string
	PublicBaseURL string
	MockBaseURL   string
	MockAdminKey  string

	// FanoutMocks are additional partner instances (VoltCity/ChargeX) kept registered so a push shows
	// the fanout hitting several parties at once. The demo drives them only through handshake /
	// unregister — PLG stays the interactive one. Empty list = single-partner demo as before.
	FanoutMocks []FanoutMock

	EvoltVersionsURL string
	EvoltOrchURL     string
	EvoltAdapterURL  string
	EvoltRoamingURL  string
	EvoltCoreAuthURL string
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
	// KafkaOCPIEvseUID is the uid Evolt addresses the EVSE by in the OCPI path, which is NOT
	// KafkaEvseUID: Evolt publishes evses.evse_id (numeric, "29") as the OCPI uid and evses.uid
	// ("EVSE-TH-000029") as the OCPI evse_id. Seeding the PATCH baseline with the wrong one of the
	// two answers every status event with 2003.
	KafkaOCPIEvseUID string
	KafkaPartyCC     string
	KafkaPartyID     string

	// EvoltDBDSN + KafkaEvseID drive the demo-only charger simulator: when set, the evse-status flow
	// flips the one configured EVSE online in Evolt's DB so the pushed status is a real value, not the
	// UNKNOWN a dev charger reports. Empty DSN = feature off (status stays whatever the DB holds).
	EvoltDBDSN string

	// PartnerDBDSN points at the mock's own Postgres (own_*/received_*/registrations/request_log). Only
	// the read-only table browser and nothing else touches it; empty = partner side of the browser off.
	PartnerDBDSN string

	// BatchJobsDir holds the batch-ocpi-process binaries + config/env built by `make batch-jobs`, so
	// the Roaming Out cron buttons start the real job. Empty = those buttons stay off.
	BatchJobsDir  string
	BatchAppEnv   string
	BatchTokenKey string

	RatePostPer10s int
	RateGetPer10s  int
	Timeout        time.Duration
}

// fanoutMocks reads the optional VCT/CHX instances. A partner with no URL set is simply absent —
// the demo then runs single-partner exactly as before.
func fanoutMocks() []FanoutMock {
	var out []FanoutMock
	for _, m := range []FanoutMock{
		{Key: "vct", URL: strings.TrimRight(os.Getenv("MOCK_VCT_URL"), "/"),
			AdminKey: getenv("MOCK_VCT_ADMIN_KEY", "vct-admin-key"), PathPrefix: "/vct",
			DBDSN: os.Getenv("MOCK_VCT_DB_DSN")},
		{Key: "chx", URL: strings.TrimRight(os.Getenv("MOCK_CHX_URL"), "/"),
			AdminKey: getenv("MOCK_CHX_ADMIN_KEY", "chx-admin-key"), PathPrefix: "/chx",
			DBDSN: os.Getenv("MOCK_CHX_DB_DSN")},
	} {
		if m.URL != "" {
			out = append(out, m)
		}
	}
	return out
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
		EvoltCoreAuthURL: strings.TrimRight(os.Getenv("EVOLT_COREAUTH_URL"), "/"),
		OrchAPIKey:       os.Getenv("EVOLT_ORCH_API_KEY"),
		RoamingAPIKey:    os.Getenv("EVOLT_ROAMING_API_KEY"),
		KafkaEnabled:     boolenvDefault("DEMO_ENABLE_KAFKA", true),
		KafkaBroker:      getenv("DEMO_KAFKA_BROKER", "redpanda-0.np.th.evtech.dev:9094"),
		KafkaTopic:       getenv("DEMO_KAFKA_TOPIC", "dev.aurora.ocpi.event.evse_status"),
		KafkaPartition:   intenv("DEMO_KAFKA_PARTITION", 0),
		KafkaStationID:   os.Getenv("DEMO_KAFKA_STATION_ID"),
		KafkaEvseUID:     os.Getenv("DEMO_KAFKA_EVSE_UID"),
		KafkaEvseID:      os.Getenv("DEMO_KAFKA_EVSE_ID"),
		KafkaOCPIEvseUID: os.Getenv("DEMO_KAFKA_OCPI_EVSE_UID"),
		EvoltDBDSN:       os.Getenv("EVOLT_DB_DSN"),
		PartnerDBDSN:     os.Getenv("PARTNER_DB_DSN"),
		FanoutMocks:      fanoutMocks(),
		BatchJobsDir:     os.Getenv("BATCH_JOBS_DIR"),
		BatchAppEnv:      getenv("BATCH_APP_ENV", "dev"),
		BatchTokenKey:    os.Getenv("BATCH_TOKEN_ENCRYPTION_KEY"),
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
	if cfg.KafkaOCPIEvseUID == "" {
		cfg.KafkaOCPIEvseUID = cfg.KafkaEvseUID
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
