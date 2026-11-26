package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthAndMetricsTransitions(t *testing.T) {
	t0 := time.Now().UTC()
	tr := NewTracker(t0)
	h := Handler(tr, 36*time.Hour)

	// Fresh startup before first scheduled run is healthy (200)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on fresh startup, got %d", rec.Code)
	}

	// Failure turns healthcheck into 503
	tr.RecordFailure(t0.Add(time.Minute), 2*time.Second, errors.New("pg_dump exited 1"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 after failure, got %d", rec.Code)
	}

	// Success restores 200 and exports Prometheus metrics
	tr.RecordSuccess(t0.Add(2*time.Minute), 1500*time.Millisecond, []ArtifactSummary{{Name: "appdb", SizeBytes: 1024}})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ad_pg_backup_up 1") {
		t.Fatalf("unexpected metrics response (%d): %s", rec.Code, rec.Body.String())
	}

	// Stale tracker triggers 503
	staleTracker := NewTracker(t0.Add(-40 * time.Hour))
	_, healthy := staleTracker.Snapshot(t0, 36*time.Hour)
	if healthy {
		t.Fatal("expected stale snapshot to be unhealthy")
	}
}
