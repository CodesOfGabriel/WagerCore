package telemetry

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	registry                           *prometheus.Registry
	transactions                       *prometheus.CounterVec
	duration                           *prometheus.HistogramVec
	replays, conflicts, reconciliation prometheus.Counter
	Retries                            *prometheus.CounterVec
	DLQ                                prometheus.Gauge
	Pending, Oldest                    prometheus.Gauge
	Published                          prometheus.Counter
	LockWait, TransactionStart         prometheus.Histogram
	Concurrency                        prometheus.Counter
}

func New() *Metrics {
	m := &Metrics{
		registry:         prometheus.NewRegistry(),
		transactions:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "wager_transactions_total", Help: "Committed operations by kind and status."}, []string{"kind", "status"}),
		duration:         prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "wager_processing_duration_seconds", Help: "Time spent processing financial requests.", Buckets: prometheus.DefBuckets}, []string{"kind"}),
		replays:          prometheus.NewCounter(prometheus.CounterOpts{Name: "wager_idempotent_replays_total", Help: "Persisted results returned for repeated operations."}),
		conflicts:        prometheus.NewCounter(prometheus.CounterOpts{Name: "wager_idempotency_conflicts_total", Help: "Identity or payload conflicts."}),
		reconciliation:   prometheus.NewCounter(prometheus.CounterOpts{Name: "wager_reconciliation_mismatches_total", Help: "Reconciliation results with a balance mismatch."}),
		Retries:          prometheus.NewCounterVec(prometheus.CounterOpts{Name: "wager_worker_retries_total", Help: "Worker attempts scheduled for retry."}, []string{"worker"}),
		DLQ:              prometheus.NewGauge(prometheus.GaugeOpts{Name: "wager_dlq_messages", Help: "Approximate messages waiting in the dead-letter queue."}),
		Pending:          prometheus.NewGauge(prometheus.GaugeOpts{Name: "wager_outbox_pending_events", Help: "Committed events awaiting publication."}),
		Oldest:           prometheus.NewGauge(prometheus.GaugeOpts{Name: "wager_outbox_oldest_pending_seconds", Help: "Age of the oldest unpublished event."}),
		Published:        prometheus.NewCounter(prometheus.CounterOpts{Name: "wager_outbox_published_total", Help: "Successful outbox confirmations."}),
		LockWait:         prometheus.NewHistogram(prometheus.HistogramOpts{Name: "wager_wallet_lock_duration_seconds", Help: "Database round trip including wallet row lock wait.", Buckets: prometheus.DefBuckets}),
		TransactionStart: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "wager_database_transaction_start_seconds", Help: "Pool acquisition and BEGIN round trip.", Buckets: prometheus.DefBuckets}),
		Concurrency:      prometheus.NewCounter(prometheus.CounterOpts{Name: "wager_concurrency_conflicts_total", Help: "Database deadlocks, serialization failures and wallet version conflicts."}),
	}
	m.registry.MustRegister(m.transactions, m.duration, m.replays, m.conflicts, m.reconciliation, m.Retries, m.DLQ, m.Pending, m.Oldest, m.Published, m.LockWait, m.TransactionStart, m.Concurrency, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}
func (m *Metrics) Processed(kind, status string, duration time.Duration) {
	m.transactions.WithLabelValues(kind, status).Inc()
	m.duration.WithLabelValues(kind).Observe(duration.Seconds())
}
func (m *Metrics) Replay()             { m.replays.Inc() }
func (m *Metrics) Conflict()           { m.conflicts.Inc() }
func (m *Metrics) Retry(worker string) { m.Retries.WithLabelValues(worker).Inc() }
func (m *Metrics) Reconciled(consistent bool) {
	if !consistent {
		m.reconciliation.Inc()
	}
}
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
