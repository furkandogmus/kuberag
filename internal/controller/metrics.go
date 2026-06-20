package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ingestionsTotal counts ingestion Job outcomes per KnowledgeBase.
	ingestionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rag_knowledgebase_ingestions_total",
			Help: "Total ingestion jobs completed, partitioned by result.",
		},
		[]string{"knowledgebase", "result"},
	)

	// indexedChunks reports the current chunk count per KnowledgeBase.
	indexedChunks = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rag_knowledgebase_indexed_chunks",
			Help: "Number of chunks currently indexed.",
		},
		[]string{"knowledgebase"},
	)

	// retrievalRecall reports the last measured recall percentage per KnowledgeBase.
	retrievalRecall = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rag_knowledgebase_recall_percent",
			Help: "Last measured retrieval recall@k as a percentage.",
		},
		[]string{"knowledgebase"},
	)

	// autoTuneAttempts reports auto-tune iterations applied per KnowledgeBase.
	autoTuneAttempts = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rag_knowledgebase_autotune_attempts",
			Help: "Number of auto-tune iterations applied.",
		},
		[]string{"knowledgebase"},
	)

	// autoTuneBestRecall reports the best recall observed across auto-tune attempts.
	autoTuneBestRecall = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rag_knowledgebase_autotune_best_recall_percent",
			Help: "Best retrieval recall@k observed across auto-tune attempts.",
		},
		[]string{"knowledgebase"},
	)

	// autoTuneDurationSeconds tracks the wall-clock duration of each
	// completed auto-tune run, per result (converged, exhausted, reset).
	// Use it as a histogram to build SLO panels like "p95 auto-tune
	// convergence time" and to alert when a run exceeds expected
	// duration.
	autoTuneDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rag_knowledgebase_autotune_duration_seconds",
			Help:    "Duration of auto-tune runs (start to settle/reset), per result.",
			Buckets: prometheus.ExponentialBuckets(30, 2, 12), // 30s .. ~17h
		},
		[]string{"knowledgebase", "result"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		ingestionsTotal, indexedChunks, retrievalRecall, autoTuneAttempts, autoTuneBestRecall,
		autoTuneDurationSeconds,
	)
}
