package controller

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ingestionsTotal counts ingestion Job outcomes per namespace.
	ingestionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rag_knowledgebase_ingestions_total",
			Help: "Total ingestion jobs completed, partitioned by result.",
		},
		[]string{"namespace", "result"},
	)

	// indexedChunks reports the current chunk count per namespace.
	indexedChunks = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rag_knowledgebase_indexed_chunks",
			Help: "Number of chunks currently indexed.",
		},
		[]string{"namespace"},
	)

	// lastSuccessfulIngestionTimestamp reports the latest successful ingestion
	// observed in a namespace. Namespace aggregation keeps cardinality bounded.
	lastSuccessfulIngestionTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rag_knowledgebase_last_successful_ingestion_timestamp_seconds",
			Help: "Unix timestamp of the latest successful KnowledgeBase ingestion in the namespace.",
		},
		[]string{"namespace"},
	)

	// retrievalRecall reports the last measured recall percentage per namespace.
	retrievalRecall = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rag_knowledgebase_recall_percent",
			Help: "Last measured retrieval recall@k as a percentage.",
		},
		[]string{"namespace"},
	)

	// autoTuneAttempts reports auto-tune iterations applied per namespace.
	autoTuneAttempts = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rag_knowledgebase_autotune_attempts",
			Help: "Number of auto-tune iterations applied.",
		},
		[]string{"namespace"},
	)

	// autoTuneBestRecall reports the best recall observed across auto-tune attempts.
	autoTuneBestRecall = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rag_knowledgebase_autotune_best_recall_percent",
			Help: "Best retrieval recall@k observed across auto-tune attempts.",
		},
		[]string{"namespace"},
	)

	// autoTuneDurationSeconds tracks the wall-clock duration of each
	// completed auto-tune run, per result (converged, exhausted, reset).
	autoTuneDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rag_knowledgebase_autotune_duration_seconds",
			Help:    "Duration of auto-tune runs (start to settle/reset), per result.",
			Buckets: prometheus.ExponentialBuckets(30, 2, 12),
		},
		[]string{"namespace", "result"},
	)

	freshnessMu                        sync.Mutex
	lastSuccessfulIngestionByNamespace = map[string]int64{}
)

func init() {
	metrics.Registry.MustRegister(
		ingestionsTotal, indexedChunks, lastSuccessfulIngestionTimestamp,
		retrievalRecall, autoTuneAttempts, autoTuneBestRecall,
		autoTuneDurationSeconds,
	)
}

func observeSuccessfulIngestion(namespace string, timestamp time.Time) {
	seconds := timestamp.Unix()
	freshnessMu.Lock()
	defer freshnessMu.Unlock()
	if lastSuccessfulIngestionByNamespace[namespace] >= seconds {
		return
	}
	lastSuccessfulIngestionByNamespace[namespace] = seconds
	lastSuccessfulIngestionTimestamp.WithLabelValues(namespace).Set(float64(seconds))
}
