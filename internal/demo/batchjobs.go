package demo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// batchJobs lists the cmd/<job> binaries the demo may start. The job name arrives in a URL path, so
// it must match one of these before it ever reaches exec — no path is ever built from raw input.
var batchJobs = []string{"locations_pull", "tariffs_pull"}

// batchLogFields are the log fields worth showing in the UI. Anything else the job prints is dropped
// rather than forwarded blindly, so a future field can never carry a token into the browser.
var batchLogFields = []string{
	"partner_name", "country_code", "party_id",
	"partners", "locations", "evses", "connectors", "tariffs",
	"skipped", "failed", "dryRun",
	"pulled", "total_count", "max_pages", "error",
}

const (
	batchMaxLogLines = 200
	batchTimeout     = 3 * time.Minute
)

// BatchRunner starts the real batch-ocpi-process binaries — the same ones the k8s CronJob runs — from
// a read-only mount, so pressing the button exercises the shipped job instead of a re-implementation
// of it. Build them with `make batch-jobs` in this repo; without that mount the runner reports itself
// unavailable and every call fails closed.
type BatchRunner struct {
	dir string
	cfg Config
}

func NewBatchRunner(cfg Config) *BatchRunner {
	if cfg.BatchJobsDir == "" {
		return nil
	}
	return &BatchRunner{dir: cfg.BatchJobsDir, cfg: cfg}
}

type BatchResult struct {
	Job        string         `json:"job"`
	DryRun     bool           `json:"dry_run"`
	ExitCode   int            `json:"exit_code"`
	DurationMS int64          `json:"duration_ms"`
	Summary    map[string]any `json:"summary,omitempty"`
	Log        []string       `json:"log"`
}

// Jobs reports the jobs that are actually runnable right now, for the UI to enable its buttons.
func (r *BatchRunner) Jobs() []string {
	if r == nil {
		return nil
	}
	out := []string{}
	for _, job := range batchJobs {
		if r.available(job) == nil {
			out = append(out, job)
		}
	}
	return out
}

var errBatchUnavailable = errors.New("batch jobs not built")

func (r *BatchRunner) available(job string) error {
	if r == nil {
		return fmt.Errorf("%w: BATCH_JOBS_DIR is not set", errBatchUnavailable)
	}
	if !contains(batchJobs, job) {
		return fmt.Errorf("unknown job %q", job)
	}
	if _, err := os.Stat(r.binPath(job)); err != nil {
		return fmt.Errorf("%w: run `make batch-jobs` first", errBatchUnavailable)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "config", "env")); err != nil {
		return fmt.Errorf("%w: config/env missing from the mount", errBatchUnavailable)
	}
	return nil
}

func (r *BatchRunner) binPath(job string) string {
	return filepath.Join(r.dir, "bin", job)
}

// Run executes one job to completion. A non-zero exit is a result, not a transport error: the job
// reporting "every partner failed" is exactly what the demo is there to show.
func (r *BatchRunner) Run(ctx context.Context, job string, dryRun bool) (*BatchResult, error) {
	if err := r.available(job); err != nil {
		return nil, err
	}
	if r.cfg.EvoltCoreAuthURL == "" || r.cfg.EvoltAdapterURL == "" || r.cfg.EvoltRoamingURL == "" {
		return nil, errors.New("EVOLT_COREAUTH_URL / EVOLT_ADAPTER_URL / EVOLT_ROAMING_URL must all be set")
	}
	if r.cfg.BatchTokenKey == "" {
		return nil, errors.New("BATCH_TOKEN_ENCRYPTION_KEY is not set — the job cannot decrypt the partner token")
	}

	ctx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()

	args := []string{}
	if dryRun {
		args = append(args, "-dry-run")
	}
	cmd := exec.CommandContext(ctx, r.binPath(job), args...)
	// The job resolves ./config/env against its working directory, so it has to run from the mount.
	cmd.Dir = r.dir
	cmd.Env = r.childEnv()

	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	started := time.Now()
	runErr := cmd.Run()
	result := &BatchResult{Job: job, DryRun: dryRun, DurationMS: time.Since(started).Milliseconds()}
	result.Log, result.Summary = parseBatchOutput(out.Bytes())

	var exit *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exit):
		result.ExitCode = exit.ExitCode()
	case ctx.Err() != nil:
		return nil, fmt.Errorf("job %s timed out after %s", job, batchTimeout)
	default:
		return nil, fmt.Errorf("start job %s: %w", job, runErr)
	}
	return result, nil
}

// childEnv passes only what the job needs. The parent environment is deliberately not inherited: it
// holds the mock admin key and Evolt's orch key, neither of which the batch has any business reading.
func (r *BatchRunner) childEnv() []string {
	env := []string{
		"APP_ENV=" + r.cfg.BatchAppEnv,
		"EXTERNAL_CORE_AUTH_BASE_URL=" + r.cfg.EvoltCoreAuthURL,
		"EXTERNAL_ADAPTER_OCPI_BASE_URL=" + r.cfg.EvoltAdapterURL,
		"EXTERNAL_CORE_OCPI_ROAMING_BASE_URL=" + r.cfg.EvoltRoamingURL,
		"EXTERNAL_CORE_OCPI_ROAMING_API_KEY=" + r.cfg.RoamingAPIKey,
		"CRYPTO_TOKEN_ENCRYPTION_KEY=" + r.cfg.BatchTokenKey,
	}
	// The job pins Asia/Bangkok at startup and a distroless image carries no tzdata; make batch-jobs
	// drops a zoneinfo.zip next to the binaries for exactly this.
	if zi := filepath.Join(r.dir, "zoneinfo.zip"); fileExists(zi) {
		env = append(env, "ZONEINFO="+zi)
	}
	return env
}

// parseBatchOutput turns the job's zerolog lines into something the UI can print, and lifts the
// closing summary out of them. Unparsable lines are kept verbatim — a Go panic is not JSON, and that
// is the one output nobody wants silently swallowed.
func parseBatchOutput(raw []byte) ([]string, map[string]any) {
	lines := []string{}
	var summary map[string]any

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			lines = append(lines, truncate(line, 300))
			continue
		}
		msg, _ := entry["message"].(string)
		if msg == "Logger initialized." {
			continue
		}
		fields := map[string]any{}
		for _, k := range batchLogFields {
			if v, present := entry[k]; present {
				fields[k] = v
			}
		}
		if msg == "batch completed" {
			summary = fields
		}
		level, _ := entry["level"].(string)
		lines = append(lines, truncate(formatBatchLine(level, msg, fields), 300))
	}
	if len(lines) > batchMaxLogLines {
		lines = append(lines[:batchMaxLogLines], fmt.Sprintf("… (%d more lines)", len(lines)-batchMaxLogLines))
	}
	return lines, summary
}

func formatBatchLine(level, msg string, fields map[string]any) string {
	var b strings.Builder
	if level != "" {
		b.WriteString(strings.ToUpper(level))
		b.WriteString(" ")
	}
	b.WriteString(msg)
	first := true
	for _, k := range batchLogFields {
		v, present := fields[k]
		if !present {
			continue
		}
		if first {
			b.WriteString(" |")
			first = false
		}
		fmt.Fprintf(&b, " %s=%v", k, v)
	}
	return b.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
