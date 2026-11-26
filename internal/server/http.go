package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ArtifactSummary records metadata about one backed-up artifact from the last cycle.
type ArtifactSummary struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Encrypted bool   `json:"encrypted"`
	S3Synced  bool   `json:"s3_synced"`
}

// Snapshot represents the current observable health state of the backup daemon.
type Snapshot struct {
	Status         string            `json:"status"`
	StartedAt      time.Time         `json:"started_at"`
	LastRunAt      *time.Time        `json:"last_run_at,omitempty"`
	LastSuccessAt  *time.Time        `json:"last_success_at,omitempty"`
	LastError      string            `json:"last_error,omitempty"`
	LastDurationMs int64             `json:"last_duration_ms"`
	TotalRuns      int64             `json:"total_runs"`
	TotalFailures  int64             `json:"total_failures"`
	Artifacts      []ArtifactSummary `json:"artifacts,omitempty"`
}

// Tracker stores backup execution health safely across goroutines.
type Tracker struct {
	mu        sync.RWMutex
	startedAt time.Time
	lastRun   time.Time
	lastOK    time.Time
	lastErr   string
	lastDur   time.Duration
	runs      int64
	failures  int64
	artifacts []ArtifactSummary
}

// NewTracker initializes a health tracker anchored at now.
func NewTracker(now time.Time) *Tracker {
	return &Tracker{startedAt: now.UTC()}
}

// RecordSuccess updates state after a clean backup cycle.
func (t *Tracker) RecordSuccess(at time.Time, dur time.Duration, artifacts []ArtifactSummary) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.runs++
	t.lastRun = at.UTC()
	t.lastOK = at.UTC()
	t.lastErr = ""
	t.lastDur = dur
	t.artifacts = append([]ArtifactSummary(nil), artifacts...)
}

// RecordFailure updates state after a failed backup cycle.
func (t *Tracker) RecordFailure(at time.Time, dur time.Duration, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.runs++
	t.failures++
	t.lastRun = at.UTC()
	t.lastDur = dur
	if err != nil {
		t.lastErr = err.Error()
	} else {
		t.lastErr = "unknown error"
	}
}

// Snapshot evaluates health against maxStale and returns (snapshot, isHealthy).
func (t *Tracker) Snapshot(now time.Time, maxStale time.Duration) (Snapshot, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	snap := Snapshot{
		Status:         "ok",
		StartedAt:      t.startedAt,
		LastError:      t.lastErr,
		LastDurationMs: t.lastDur.Milliseconds(),
		TotalRuns:      t.runs,
		TotalFailures:  t.failures,
		Artifacts:      append([]ArtifactSummary(nil), t.artifacts...),
	}
	if !t.lastRun.IsZero() {
		v := t.lastRun
		snap.LastRunAt = &v
	}
	if !t.lastOK.IsZero() {
		v := t.lastOK
		snap.LastSuccessAt = &v
	}

	healthy := true
	if t.lastErr != "" {
		healthy = false
	} else if t.runs > 0 && t.lastOK.IsZero() {
		healthy = false
	} else if maxStale > 0 {
		ref := t.startedAt
		if !t.lastOK.IsZero() {
			ref = t.lastOK
		}
		if now.UTC().Sub(ref) > maxStale {
			healthy = false
			if snap.LastError == "" {
				snap.LastError = fmt.Sprintf("backup stale for >%s", maxStale)
			}
		}
	}

	if !healthy {
		snap.Status = "error"
	}
	return snap, healthy
}

// Handler serves /healthz, /, and Prometheus /metrics endpoints.
func Handler(tracker *Tracker, maxStale time.Duration) http.Handler {
	mux := http.NewServeMux()
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		snap, ok := tracker.Snapshot(time.Now().UTC(), maxStale)
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(snap)
	}
	mux.HandleFunc("/", healthHandler)
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		snap, ok := tracker.Snapshot(time.Now().UTC(), maxStale)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		up := 0
		if ok {
			up = 1
		}
		var lastSuccessSec int64
		if snap.LastSuccessAt != nil {
			lastSuccessSec = snap.LastSuccessAt.Unix()
		}
		_, _ = fmt.Fprintf(w,
			"# TYPE ad_pg_backup_up gauge\nad_pg_backup_up %d\n"+
				"# TYPE ad_pg_backup_runs_total counter\nad_pg_backup_runs_total %d\n"+
				"# TYPE ad_pg_backup_failures_total counter\nad_pg_backup_failures_total %d\n"+
				"# TYPE ad_pg_backup_last_success_timestamp_seconds gauge\nad_pg_backup_last_success_timestamp_seconds %d\n"+
				"# TYPE ad_pg_backup_last_duration_seconds gauge\nad_pg_backup_last_duration_seconds %.3f\n",
			up, snap.TotalRuns, snap.TotalFailures, lastSuccessSec, float64(snap.LastDurationMs)/1000.0,
		)
	})
	return mux
}
