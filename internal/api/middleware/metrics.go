package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// HTTP metrics
	requestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "endpoint", "status"},
	)

	requestCount = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "endpoint", "status"},
	)

	// Business metrics — exported for use by handlers/allocator/releaser
	AllocationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "smart_router_allocations_total",
			Help: "Total allocations by pool and status",
		},
		[]string{"pool", "status"},
	)

	ReleasesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "smart_router_releases_total",
			Help: "Total releases by pool and status",
		},
		[]string{"pool", "status"},
	)

	ActiveCalls = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "smart_router_active_calls",
			Help: "Current number of active calls",
		},
	)

	PoolAvailablePods = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "smart_router_pool_available_pods",
			Help: "Available pods per tier",
		},
		[]string{"tier"},
	)

	PoolAssignedPods = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "smart_router_pool_assigned_pods",
			Help: "Assigned pods per tier",
		},
		[]string{"tier"},
	)

	LeaderStatus = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "smart_router_leader_status",
			Help: "Whether this instance is the leader (1) or not (0)",
		},
	)

	PanicsRecoveredTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "smart_router_panics_recovered_total",
			Help: "Total number of recovered panics",
		},
	)

	DrainsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "smart_router_drains_total",
			Help: "Total number of drain operations",
		},
	)

	ZombiesRecoveredTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "smart_router_zombies_recovered_total",
			Help: "Total number of zombie pods recovered",
		},
	)
)

// Metrics returns a middleware that collects Prometheus metrics
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap response writer to capture status code
		wrapped := &metricsResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)
		status := strconv.Itoa(wrapped.statusCode)

		// Use Chi route pattern to avoid cardinality explosion from dynamic path segments
		endpoint := r.URL.Path
		if rctx := chi.RouteContext(r.Context()); rctx != nil {
			if pattern := rctx.RoutePattern(); pattern != "" {
				endpoint = pattern
			}
		}
		// Normalize trailing slashes
		endpoint = strings.TrimRight(endpoint, "/")
		if endpoint == "" {
			endpoint = "/"
		}

		// Record metrics
		requestDuration.WithLabelValues(r.Method, endpoint, status).Observe(duration.Seconds())
		requestCount.WithLabelValues(r.Method, endpoint, status).Inc()
	})
}

// metricsResponseWriter wraps http.ResponseWriter to capture status code
type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *metricsResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
