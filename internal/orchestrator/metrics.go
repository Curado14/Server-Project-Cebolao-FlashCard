package orchestrator

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

type metricsRegistry struct {
	photosAccepted  atomic.Int64
	photosRejected  atomic.Int64
	jobsActive      atomic.Int64
	jobsCompleted   atomic.Int64
	jobsFailed      atomic.Int64
	jobsQuarantined atomic.Int64
	sqliteErrors    atomic.Int64
	loggingErrors   atomic.Int64

	mu            sync.Mutex
	httpDuration  histogram
	persistMillis histogram
	storageFree   uint64
}

type histogram struct {
	buckets []float64
	counts  []uint64
	count   uint64
	sum     float64
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{
		httpDuration:  newHistogram([]float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}),
		persistMillis: newHistogram([]float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}),
	}
}

func newHistogram(buckets []float64) histogram {
	return histogram{buckets: buckets, counts: make([]uint64, len(buckets))}
}

func (m *metricsRegistry) observeHTTPDuration(ms float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.httpDuration.observe(ms)
}

func (m *metricsRegistry) observePersistDuration(ms float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.persistMillis.observe(ms)
}

func (m *metricsRegistry) setStorageFree(bytes uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storageFree = bytes
}

func (h *histogram) observe(value float64) {
	h.count++
	h.sum += value
	for i, bucket := range h.buckets {
		if value <= bucket {
			h.counts[i]++
		}
	}
}

func (s *Service) writeMetrics(w io.Writer) {
	s.metricsRegistry.mu.Lock()
	httpDuration := s.metricsRegistry.httpDuration
	persistMillis := s.metricsRegistry.persistMillis
	storageFree := s.metricsRegistry.storageFree
	s.metricsRegistry.mu.Unlock()

	writeCounter(w, "photoedge_photos_accepted_total", "Photos accepted after durable persistence.", s.metricsRegistry.photosAccepted.Load())
	writeCounter(w, "photoedge_photos_rejected_total", "Photos rejected before durable acceptance.", s.metricsRegistry.photosRejected.Load())
	writeHistogram(w, "photoedge_http_request_duration_ms", "HTTP request duration in milliseconds.", httpDuration)
	writeHistogram(w, "photoedge_photo_persist_duration_ms", "Original photo persistence duration in milliseconds.", persistMillis)
	writeGauge(w, "photoedge_queue_depth", "Jobs waiting for processing.", s.queueDepth())
	writeGauge(w, "photoedge_jobs_active", "Jobs currently executing.", s.metricsRegistry.jobsActive.Load())
	writeCounter(w, "photoedge_jobs_completed_total", "Jobs completed successfully.", s.metricsRegistry.jobsCompleted.Load())
	writeCounter(w, "photoedge_jobs_failed_total", "Jobs that failed and will retry or are dead.", s.metricsRegistry.jobsFailed.Load())
	writeCounter(w, "photoedge_jobs_quarantined_total", "Jobs or files moved to quarantine.", s.metricsRegistry.jobsQuarantined.Load())
	writeGauge(w, "photoedge_storage_free_bytes", "Free bytes available under the storage filesystem.", int64(storageFree))
	writeCounter(w, "photoedge_sqlite_errors_total", "SQLite operation errors observed by the service.", s.metricsRegistry.sqliteErrors.Load())
	writeCounter(w, "photoedge_logging_errors_total", "Diagnostic logging write errors observed by the service.", s.metricsRegistry.loggingErrors.Load())
	writeCounter(w, "photoedge_accepted_total", "Temporary alias for photoedge_photos_accepted_total.", s.metricsRegistry.photosAccepted.Load())
	writeCounter(w, "photoedge_rejected_total", "Temporary alias for photoedge_photos_rejected_total.", s.metricsRegistry.photosRejected.Load())
}

func writeCounter(w io.Writer, name, help string, value int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, value)
}

func writeGauge(w io.Writer, name, help string, value int64) {
	if value < 0 {
		value = 0
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, value)
}

func writeHistogram(w io.Writer, name, help string, h histogram) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for i, bucket := range h.buckets {
		fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", name, bucket, h.counts[i])
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %g\n%s_count %d\n", name, h.count, name, h.sum, name, h.count)
}

func (s *Service) queueDepth() int64 {
	var n int64
	if err := s.db.sql.QueryRow(`SELECT count(*) FROM jobs WHERE status IN ('PENDING','RETRY_WAIT')`).Scan(&n); err != nil {
		s.metricsRegistry.sqliteErrors.Add(1)
		return 0
	}
	return n
}
