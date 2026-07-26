package demo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A job name reaches exec straight from a URL path, so anything outside the whitelist — including a
// traversal attempt — must be refused before a path is ever built from it.
func TestBatchRunnerRejectsUnknownJob(t *testing.T) {
	dir := t.TempDir()
	r := NewBatchRunner(Config{BatchJobsDir: dir})
	for _, job := range []string{"", "sh", "../../bin/sh", "locations_pull/../sh", "LOCATIONS_PULL"} {
		if _, err := r.Run(context.Background(), job, false); err == nil {
			t.Fatalf("job %q was accepted", job)
		}
	}
}

func TestBatchRunnerUnavailableWithoutBinaries(t *testing.T) {
	if jobs := (*BatchRunner)(nil).Jobs(); len(jobs) != 0 {
		t.Fatalf("nil runner reported jobs: %v", jobs)
	}
	if _, err := (*BatchRunner)(nil).Run(context.Background(), "locations_pull", false); err == nil {
		t.Fatal("nil runner ran a job")
	}
	r := NewBatchRunner(Config{BatchJobsDir: t.TempDir()})
	if jobs := r.Jobs(); len(jobs) != 0 {
		t.Fatalf("empty mount reported jobs: %v", jobs)
	}
	if _, err := r.Run(context.Background(), "locations_pull", false); err == nil {
		t.Fatal("empty mount ran a job")
	}
}

// The job inherits nothing: the parent holds the mock admin key and Evolt's orch key, and neither
// belongs in a child process.
func TestBatchChildEnvCarriesOnlyWhatTheJobNeeds(t *testing.T) {
	t.Setenv("SHOULD_NOT_LEAK", "parent-secret")
	r := NewBatchRunner(Config{
		BatchJobsDir:     "/batch",
		BatchAppEnv:      "dev",
		EvoltCoreAuthURL: "http://core-auth",
		EvoltAdapterURL:  "http://adapter",
		EvoltRoamingURL:  "http://roaming",
		RoamingAPIKey:    "roaming-key",
		BatchTokenKey:    "deadbeef",
		MockAdminKey:     "mock-admin-key",
		OrchAPIKey:       "orch-key",
	})
	allowed := map[string]bool{
		"APP_ENV": true, "EXTERNAL_CORE_AUTH_BASE_URL": true, "EXTERNAL_ADAPTER_OCPI_BASE_URL": true,
		"EXTERNAL_CORE_OCPI_ROAMING_BASE_URL": true, "EXTERNAL_CORE_OCPI_ROAMING_API_KEY": true,
		"CRYPTO_TOKEN_ENCRYPTION_KEY": true, "ZONEINFO": true,
	}
	for _, kv := range r.childEnv() {
		key, value, _ := strings.Cut(kv, "=")
		if !allowed[key] {
			t.Errorf("unexpected env %q passed to the job", key)
		}
		if value == "mock-admin-key" || value == "orch-key" || value == "parent-secret" {
			t.Errorf("env %q carried a value the job must not see", key)
		}
	}
}

func TestBatchRunnerRefusesIncompleteConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "config", "env"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file that exists but is not executable stands in for a half-finished build.
	if err := os.WriteFile(filepath.Join(dir, "bin", "locations_pull"), []byte("#!/bin/false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Missing URLs and a missing key must both fail before anything is started.
	r := NewBatchRunner(Config{BatchJobsDir: dir, BatchTokenKey: "deadbeef"})
	if _, err := r.Run(context.Background(), "locations_pull", false); err == nil {
		t.Fatal("ran without upstream URLs")
	}
	r = NewBatchRunner(Config{
		BatchJobsDir:     dir,
		EvoltCoreAuthURL: "http://core-auth", EvoltAdapterURL: "http://adapter", EvoltRoamingURL: "http://roaming",
	})
	if _, err := r.Run(context.Background(), "locations_pull", false); err == nil {
		t.Fatal("ran without an encryption key")
	}
}

func TestParseBatchOutputLiftsSummaryAndDropsUnknownFields(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`{"level":"info","message":"Logger initialized."}`,
		`{"level":"info","message":"no watermark held for partner","partner_name":"PlugSiam","token":"must-not-appear"}`,
		`panic: boom`,
		`{"level":"info","message":"batch completed","partners":1,"locations":4,"failed":0,"dryRun":true}`,
	}, "\n"))

	lines, summary := parseBatchOutput(raw)
	if summary == nil || summary["locations"] != float64(4) || summary["partners"] != float64(1) {
		t.Fatalf("summary not lifted: %v", summary)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "must-not-appear") {
		t.Error("an unknown log field was forwarded to the UI")
	}
	if strings.Contains(joined, "Logger initialized") {
		t.Error("startup noise kept")
	}
	if !strings.Contains(joined, "panic: boom") {
		t.Error("non-JSON output dropped — a panic must stay visible")
	}
	if !strings.Contains(joined, "PlugSiam") {
		t.Error("whitelisted field dropped")
	}
}
