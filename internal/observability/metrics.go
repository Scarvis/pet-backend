// Package observability exposes the Prometheus metrics from ARCHITECTURE.md
// §10 and the HTTP middleware + dispatcher hooks that feed them.
package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// HTTPRequestsTotal counts Action/Event/Cron calls by handler, method, status.
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "financeapp_http_requests_total",
		Help: "Total HTTP requests by handler, method and status.",
	}, []string{"handler", "method", "status"})

	// HTTPRequestDuration observes Action/Event/Cron latency by handler.
	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "financeapp_http_request_duration_seconds",
		Help:    "HTTP request latency by handler.",
		Buckets: prometheus.DefBuckets,
	}, []string{"handler"})

	// EventsProcessedTotal counts worker-handled events by type.
	EventsProcessedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "financeapp_events_processed_total",
		Help: "Total events processed by the LISTEN worker by event type.",
	}, []string{"event_type"})

	// EventsProcessingDuration observes per-event DB-transaction time by type.
	EventsProcessingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "financeapp_events_processing_duration_seconds",
		Help:    "Event processing duration by event type.",
		Buckets: prometheus.DefBuckets,
	}, []string{"event_type"})

	// EventsFailedTotal counts poison events by type (see §8.8).
	EventsFailedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "financeapp_events_failed_total",
		Help: "Total failed events by event type.",
	}, []string{"event_type"})

	// EventsLag is max(app.events.id) − last_event_id (§10).
	EventsLag = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "financeapp_events_lag",
		Help: "Event worker lag: max event id minus consumed cursor.",
	})

	// ImportRowsTotal counts mapped import rows by job and status.
	ImportRowsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "financeapp_import_rows_total",
		Help: "Total import rows by job and status.",
	}, []string{"job_id", "status"})

	// FXFetchDuration observes the external FX fetch latency.
	FXFetchDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "financeapp_fx_fetch_duration_seconds",
		Help:    "Duration of the external FX rates fetch.",
		Buckets: prometheus.DefBuckets,
	})

	// DBPoolInUse tracks acquired connections in the pgx pool.
	DBPoolInUse = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "financeapp_db_pool_in_use",
		Help: "Number of database connections currently in use.",
	})
)

// statusRecorder captures the status code for the requests-total label.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Instrument returns middleware recording financeapp_http_requests_total and
// financeapp_http_request_duration_seconds with handler=name.
func Instrument(name string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		HTTPRequestDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())
		HTTPRequestsTotal.WithLabelValues(name, r.Method, strconv.Itoa(rec.status)).Inc()
	})
}

// ObserveEventProcessed records a successful event handling.
func ObserveEventProcessed(eventType string, d time.Duration) {
	EventsProcessedTotal.WithLabelValues(eventType).Inc()
	EventsProcessingDuration.WithLabelValues(eventType).Observe(d.Seconds())
}

// ObserveEventFailed records a poison event.
func ObserveEventFailed(eventType string) {
	EventsFailedTotal.WithLabelValues(eventType).Inc()
}

// ObserveImportRows records one import row outcome.
func ObserveImportRows(jobID, status string) {
	ImportRowsTotal.WithLabelValues(jobID, status).Inc()
}

// ObserveFXFetch records one FX fetch duration.
func ObserveFXFetch(d time.Duration) {
	FXFetchDuration.Observe(d.Seconds())
}

// SetEventsLag sets the worker lag gauge.
func SetEventsLag(lag float64) {
	EventsLag.Set(lag)
}

// SetDBPoolInUse sets the pool-in-use gauge.
func SetDBPoolInUse(n float64) {
	DBPoolInUse.Set(n)
}
