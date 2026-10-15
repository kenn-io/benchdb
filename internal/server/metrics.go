package server

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/benchdb/internal/api"
)

const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

type metricKey struct {
	method string
	route  string
	status int
}

type requestMetric struct {
	count       uint64
	durationSum float64
}

type unknownCommitCounter interface {
	UnknownCommitCount() int64
}

type metricsRecorder struct {
	mu                 sync.Mutex
	requests           map[metricKey]requestMetric
	unknownCommitCount func() int64
	storage            api.StorageStatusReader
}

func newMetricsRecorder(provider any) *metricsRecorder {
	unknownCommitCount := func() int64 { return 0 }
	if counter, ok := provider.(unknownCommitCounter); ok {
		unknownCommitCount = counter.UnknownCommitCount
	}
	return &metricsRecorder{
		requests:           make(map[metricKey]requestMetric),
		unknownCommitCount: unknownCommitCount,
	}
}

func instrumentHTTP(next http.Handler, recorder *metricsRecorder, basePath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		recorder.observe(metricMethodLabel(r.Method), routeLabel(strings.TrimPrefix(r.URL.Path, basePath)), rec.status, time.Since(start).Seconds())
	})
}

func (m *metricsRecorder) observe(method, route string, status int, durationSeconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := metricKey{method: method, route: route, status: status}
	value := m.requests[key]
	value.count++
	value.durationSum += durationSeconds
	m.requests[key] = value
}

func (m *metricsRecorder) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", metricsContentType)
	if r.Method == http.MethodHead {
		return
	}

	m.writeStorageMetrics(w, r)
	snapshot := m.snapshot()
	_, _ = fmt.Fprint(w, "# HELP benchdb_up Constant 1 when the Go server is serving requests.\n")
	_, _ = fmt.Fprint(w, "# TYPE benchdb_up gauge\n")
	_, _ = fmt.Fprint(w, "benchdb_up 1\n")
	_, _ = fmt.Fprint(w, "# HELP benchdb_github_unknown_commits_total Total commit resolves that degraded to unknown GitHub metadata since process start.\n")
	_, _ = fmt.Fprint(w, "# TYPE benchdb_github_unknown_commits_total counter\n")
	_, _ = fmt.Fprintf(w, "benchdb_github_unknown_commits_total %d\n", m.unknownCommitCount())
	_, _ = fmt.Fprint(w, "# HELP benchdb_http_requests_total Total HTTP requests by method, low-cardinality route, and status code.\n")
	_, _ = fmt.Fprint(w, "# TYPE benchdb_http_requests_total counter\n")
	for _, key := range sortedMetricKeys(snapshot) {
		labels := metricLabels(key)
		_, _ = fmt.Fprintf(w, "benchdb_http_requests_total{%s} %d\n", labels, snapshot[key].count)
	}
	_, _ = fmt.Fprint(w, "# HELP benchdb_http_request_duration_seconds HTTP request duration summary by method, low-cardinality route, and status code.\n")
	_, _ = fmt.Fprint(w, "# TYPE benchdb_http_request_duration_seconds summary\n")
	for _, key := range sortedMetricKeys(snapshot) {
		labels := metricLabels(key)
		value := snapshot[key]
		_, _ = fmt.Fprintf(w, "benchdb_http_request_duration_seconds_count{%s} %d\n", labels, value.count)
		_, _ = fmt.Fprintf(w, "benchdb_http_request_duration_seconds_sum{%s} %.9f\n", labels, value.durationSum)
	}
}

func (m *metricsRecorder) writeStorageMetrics(w http.ResponseWriter, r *http.Request) {
	gauge := func(name, help string, value any) {
		_, _ = fmt.Fprintf(w, "# HELP benchdb_storage_%s %s\n# TYPE benchdb_storage_%s gauge\nbenchdb_storage_%s %v\n", name, help, name, name, value)
	}
	if m.storage == nil {
		gauge("sample_success", "Whether storage usage is currently available.", 0)
		return
	}
	status, err := m.storage.StorageStatus(r.Context())
	if err != nil {
		gauge("sample_success", "Whether storage usage is currently available.", 0)
		return
	}
	gauge("sample_success", "Whether storage usage is currently available.", 1)
	gauge("sample_timestamp_seconds", "Unix time of the storage usage sample.", status.SampledAt.Unix())
	gauge("postgres_bytes", "PostgreSQL database size including indexes and metadata.", status.PostgresBytes)
	gauge("pending_rows_estimate", "Estimated PostgreSQL rows awaiting archival; statistics may lag changes.", status.PendingRowsEstimate)
	gauge("archived_rows", "Rows in published archive files before pending deletions.", status.ArchivedRows)
	gauge("parquet_bytes", "Bytes in published fact Parquet files.", status.ParquetBytes)
	gauge("parquet_files", "Published fact Parquet files.", status.ParquetFiles)
	gauge("catalog_bytes", "Bytes in published catalog files.", status.CatalogBytes)
	gauge("catalog_files", "Published catalog files.", status.CatalogFiles)
	gauge("pending_mutations", "Mutations awaiting archive maintenance.", status.PendingMutations)
	gauge("stale_catalogs", "Archive files whose catalogs need rebuilding.", status.StaleCatalogs)
	gauge("volume_total_bytes", "Capacity of the archive filesystem.", status.VolumeTotalBytes)
	gauge("volume_available_bytes", "Available space on the archive filesystem.", status.VolumeAvailableBytes)
	for _, state := range []struct {
		name, help string
		active     bool
	}{
		{"maintenance_enabled", "Whether automatic archive maintenance is enabled.", status.Maintenance.Enabled},
		{"maintenance_running", "Whether an archive maintenance cycle is running.", status.Maintenance.State == "running"},
		{"maintenance_failed", "Whether the last archive maintenance cycle failed.", status.Maintenance.State == "failed"},
		{"maintenance_interrupted", "Whether the last archive maintenance cycle was interrupted.", status.Maintenance.State == "interrupted"},
	} {
		value := 0
		if state.active {
			value = 1
		}
		gauge(state.name, state.help, value)
	}
	gauge("maintenance_interval_seconds", "Seconds between scheduled maintenance cycles.", status.Maintenance.IntervalSeconds)
	gauge("maintenance_exported_rows", "Rows exported in the current or most recent maintenance cycle.", status.Maintenance.ExportedRows)
	gauge("maintenance_compacted_files", "Files compacted in the current or most recent maintenance cycle.", status.Maintenance.CompactedFiles)
	gauge("maintenance_refreshed_catalogs", "Fact files whose catalog pair was refreshed in the current or most recent maintenance cycle.", status.Maintenance.RefreshedCatalogs)
	gauge("maintenance_removed_files", "Files removed in the current or most recent maintenance cycle.", status.Maintenance.RemovedFiles)
	for _, event := range []struct {
		name, help string
		at         *time.Time
	}{
		{"maintenance_next_run_timestamp_seconds", "Unix time of the next scheduled maintenance cycle.", status.Maintenance.NextRunAt},
		{"maintenance_last_started_timestamp_seconds", "Unix time when the last maintenance cycle started.", status.Maintenance.LastStartedAt},
		{"maintenance_last_finished_timestamp_seconds", "Unix time when the last maintenance cycle finished.", status.Maintenance.LastFinishedAt},
		{"maintenance_last_success_timestamp_seconds", "Unix time when the last successful maintenance cycle finished.", status.Maintenance.LastSuccessAt},
	} {
		if event.at != nil {
			gauge(event.name, event.help, event.at.Unix())
		}
	}
}

func (m *metricsRecorder) snapshot() map[metricKey]requestMetric {
	m.mu.Lock()
	defer m.mu.Unlock()

	return maps.Clone(m.requests)
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(data []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func metricLabels(key metricKey) string {
	return "method=" + prometheusQuote(key.method) +
		",route=" + prometheusQuote(key.route) +
		",status=" + prometheusQuote(strconv.Itoa(key.status))
}

func metricMethodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions:
		return method
	default:
		return "OTHER"
	}
}

func sortedMetricKeys(values map[metricKey]requestMetric) []metricKey {
	keys := make([]metricKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b metricKey) int {
		if c := strings.Compare(a.route, b.route); c != 0 {
			return c
		}
		if c := strings.Compare(a.method, b.method); c != 0 {
			return c
		}
		return a.status - b.status
	})
	return keys
}

func prometheusQuote(value string) string {
	quoted := strconv.Quote(value)
	return quoted
}

func routeLabel(path string) string {
	switch {
	case path == "/":
		return "/"
	case path == "/metrics":
		return "/metrics"
	case path == "/api/storage":
		return "/api/storage"
	case path == "/api/ping":
		return "/api/ping"
	case path == "/api/results":
		return "/api/results"
	case path == "/api/benchmark-results":
		return "/api/benchmark-results"
	case strings.HasPrefix(path, "/api/benchmark-results/"):
		return "/api/benchmark-results/{id}"
	case path == "/api/history":
		return "/api/history"
	case strings.HasPrefix(path, "/api/history/"):
		return "/api/history/{benchmark_result_id}"
	case path == "/api/series":
		return "/api/series"
	case path == "/api/compare/benchmark-results":
		return "/api/compare/benchmark-results"
	case path == "/api/ci/report":
		return "/api/ci/report"
	case path == "/api/alert-rules":
		return "/api/alert-rules"
	case strings.HasPrefix(path, "/api/alert-rules/") && strings.HasSuffix(path, "/events"):
		return "/api/alert-rules/{id}/events"
	case strings.HasPrefix(path, "/api/alert-rules/"):
		return "/api/alert-rules/{id}"
	case strings.HasPrefix(path, "/api/auth/"):
		return "/api/auth/*"
	case path == "/api/tokens":
		return "/api/tokens"
	case path == "/api/users/me":
		return "/api/users/me"
	case strings.HasPrefix(path, "/api/"):
		return "/api/*"
	case strings.HasPrefix(path, "/docs"):
		return "/docs"
	case strings.HasPrefix(path, "/openapi"):
		return "/openapi"
	default:
		return "spa"
	}
}
