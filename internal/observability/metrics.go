package observability

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const requestDurationWindow = 512

type Metrics struct {
	startedAt      time.Time
	httpRequests   atomic.Uint64
	httpFailures   atomic.Uint64
	httpInFlight   atomic.Int64
	mutations      atomic.Uint64
	queueRejected  atomic.Uint64
	jobsReconciled atomic.Uint64
	durationMu     sync.Mutex
	durations      [requestDurationWindow]uint64
	durationCount  int
	durationNext   int
}

type Snapshot struct {
	StartedAt       time.Time `json:"started_at"`
	UptimeSeconds   int64     `json:"uptime_seconds"`
	HTTPRequests    uint64    `json:"http_requests"`
	HTTPFailures    uint64    `json:"http_failures"`
	HTTPInFlight    int64     `json:"http_in_flight"`
	DurationSamples int       `json:"duration_samples"`
	AverageHTTPMS   float64   `json:"average_http_ms"`
	P95HTTPMS       float64   `json:"p95_http_ms"`
	P99HTTPMS       float64   `json:"p99_http_ms"`
	MaxHTTPMS       float64   `json:"max_http_ms"`
	Mutations       uint64    `json:"mutations"`
	QueueRejected   uint64    `json:"queue_rejected"`
	JobsReconciled  uint64    `json:"jobs_reconciled"`
}

func New() *Metrics { return &Metrics{startedAt: time.Now().UTC()} }

func (m *Metrics) RequestStarted() { m.httpInFlight.Add(1) }

func (m *Metrics) RequestCompleted(status int, duration time.Duration, mutation bool) {
	m.httpInFlight.Add(-1)
	m.httpRequests.Add(1)
	m.durationMu.Lock()
	m.durations[m.durationNext] = uint64(duration)
	m.durationNext = (m.durationNext + 1) % requestDurationWindow
	if m.durationCount < requestDurationWindow {
		m.durationCount++
	}
	m.durationMu.Unlock()
	if status >= http.StatusInternalServerError {
		m.httpFailures.Add(1)
	}
	if mutation && status >= 200 && status < 300 {
		m.mutations.Add(1)
	}
}

func (m *Metrics) QueueRejected() { m.queueRejected.Add(1) }
func (m *Metrics) JobsReconciled(count int) {
	if count > 0 {
		m.jobsReconciled.Add(uint64(count))
	}
}

func (m *Metrics) Snapshot() Snapshot {
	requests := m.httpRequests.Load()
	samples, average, p95, p99, maximum := m.durationStats()
	return Snapshot{
		StartedAt: m.startedAt, UptimeSeconds: int64(time.Since(m.startedAt).Seconds()),
		HTTPRequests: requests, HTTPFailures: m.httpFailures.Load(), HTTPInFlight: m.httpInFlight.Load(),
		DurationSamples: samples, AverageHTTPMS: average, P95HTTPMS: p95, P99HTTPMS: p99, MaxHTTPMS: maximum,
		Mutations: m.mutations.Load(), QueueRejected: m.queueRejected.Load(),
		JobsReconciled: m.jobsReconciled.Load(),
	}
}

func (m *Metrics) durationStats() (int, float64, float64, float64, float64) {
	m.durationMu.Lock()
	values := append([]uint64(nil), m.durations[:m.durationCount]...)
	m.durationMu.Unlock()
	if len(values) == 0 {
		return 0, 0, 0, 0, 0
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	percentile := func(percent int) float64 {
		index := (percent*len(values) + 99) / 100
		if index < 1 {
			index = 1
		}
		return float64(values[index-1]) / float64(time.Millisecond)
	}
	average := float64(total) / float64(time.Millisecond) / float64(len(values))
	maximum := float64(values[len(values)-1]) / float64(time.Millisecond)
	return len(values), average, percentile(95), percentile(99), maximum
}

func (m *Metrics) WritePrometheus(w http.ResponseWriter) {
	snapshot := m.Snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprintf(w, "hermes_fleet_http_requests_total %d\n", snapshot.HTTPRequests)
	_, _ = fmt.Fprintf(w, "hermes_fleet_http_failures_total %d\n", snapshot.HTTPFailures)
	_, _ = fmt.Fprintf(w, "hermes_fleet_http_in_flight %d\n", snapshot.HTTPInFlight)
	_, _ = fmt.Fprintf(w, "hermes_fleet_http_duration_samples %d\n", snapshot.DurationSamples)
	_, _ = fmt.Fprintf(w, "hermes_fleet_http_duration_milliseconds_average %.3f\n", snapshot.AverageHTTPMS)
	_, _ = fmt.Fprintf(w, "hermes_fleet_http_duration_milliseconds_p95 %.3f\n", snapshot.P95HTTPMS)
	_, _ = fmt.Fprintf(w, "hermes_fleet_http_duration_milliseconds_p99 %.3f\n", snapshot.P99HTTPMS)
	_, _ = fmt.Fprintf(w, "hermes_fleet_http_duration_milliseconds_max %.3f\n", snapshot.MaxHTTPMS)
	_, _ = fmt.Fprintf(w, "hermes_fleet_mutations_total %d\n", snapshot.Mutations)
	_, _ = fmt.Fprintf(w, "hermes_fleet_queue_rejected_total %d\n", snapshot.QueueRejected)
	_, _ = fmt.Fprintf(w, "hermes_fleet_jobs_reconciled_total %d\n", snapshot.JobsReconciled)
}
