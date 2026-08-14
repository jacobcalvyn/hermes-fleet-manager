package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsSnapshotAndPrometheusOutput(t *testing.T) {
	metrics := New()
	metrics.RequestStarted()
	metrics.RequestCompleted(http.StatusOK, 10*time.Millisecond, true)
	metrics.RequestStarted()
	metrics.RequestCompleted(http.StatusInternalServerError, 30*time.Millisecond, false)
	metrics.QueueRejected()
	metrics.JobsReconciled(2)

	snapshot := metrics.Snapshot()
	if snapshot.HTTPRequests != 2 || snapshot.HTTPFailures != 1 || snapshot.Mutations != 1 {
		t.Fatalf("unexpected HTTP metrics: %+v", snapshot)
	}
	if snapshot.HTTPInFlight != 0 || snapshot.QueueRejected != 1 || snapshot.JobsReconciled != 2 {
		t.Fatalf("unexpected reliability metrics: %+v", snapshot)
	}
	if snapshot.AverageHTTPMS < 19.9 || snapshot.AverageHTTPMS > 20.1 {
		t.Fatalf("average HTTP duration = %f; want 20ms", snapshot.AverageHTTPMS)
	}
	if snapshot.DurationSamples != 2 || snapshot.P95HTTPMS != 30 || snapshot.P99HTTPMS != 30 || snapshot.MaxHTTPMS != 30 {
		t.Fatalf("HTTP duration window = %+v; want 2 samples and 30ms p95/p99/max", snapshot)
	}

	recorder := httptest.NewRecorder()
	metrics.WritePrometheus(recorder)
	body := recorder.Body.String()
	for _, expected := range []string{
		"hermes_fleet_http_requests_total 2",
		"hermes_fleet_http_failures_total 1",
		"hermes_fleet_http_duration_samples 2",
		"hermes_fleet_http_duration_milliseconds_average 20.000",
		"hermes_fleet_http_duration_milliseconds_p95 30.000",
		"hermes_fleet_http_duration_milliseconds_p99 30.000",
		"hermes_fleet_http_duration_milliseconds_max 30.000",
		"hermes_fleet_queue_rejected_total 1",
		"hermes_fleet_jobs_reconciled_total 2",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("Prometheus output missing %q:\n%s", expected, body)
		}
	}
}

func TestMetricsDurationStatisticsUseTheSameBoundedWindow(t *testing.T) {
	metrics := New()
	for index := 0; index < requestDurationWindow+10; index++ {
		duration := time.Millisecond
		if index < 10 {
			duration = time.Second
		}
		metrics.RequestStarted()
		metrics.RequestCompleted(http.StatusOK, duration, false)
	}

	snapshot := metrics.Snapshot()
	if snapshot.HTTPRequests != requestDurationWindow+10 || snapshot.DurationSamples != requestDurationWindow {
		t.Fatalf("request/window counts = %d/%d; want %d/%d", snapshot.HTTPRequests, snapshot.DurationSamples, requestDurationWindow+10, requestDurationWindow)
	}
	if snapshot.AverageHTTPMS != 1 || snapshot.P95HTTPMS != 1 || snapshot.P99HTTPMS != 1 || snapshot.MaxHTTPMS != 1 {
		t.Fatalf("bounded duration statistics = %+v; want all values from the latest 512 requests", snapshot)
	}
}
