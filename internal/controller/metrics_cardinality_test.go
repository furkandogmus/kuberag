package controller

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestOperatorMetricLabelsStayWithinCardinalityBudget(t *testing.T) {
	collectors := map[string]struct {
		collector prometheus.Collector
		labels    string
	}{
		"ingestions": {
			collector: ingestionsTotal,
			labels:    "variableLabels: {namespace,result}",
		},
		"indexed chunks": {
			collector: indexedChunks,
			labels:    "variableLabels: {namespace}",
		},
		"last successful ingestion": {
			collector: lastSuccessfulIngestionTimestamp,
			labels:    "variableLabels: {namespace}",
		},
		"recall": {
			collector: retrievalRecall,
			labels:    "variableLabels: {namespace}",
		},
		"auto-tune attempts": {
			collector: autoTuneAttempts,
			labels:    "variableLabels: {namespace}",
		},
		"auto-tune best recall": {
			collector: autoTuneBestRecall,
			labels:    "variableLabels: {namespace}",
		},
		"auto-tune duration": {
			collector: autoTuneDurationSeconds,
			labels:    "variableLabels: {namespace,result}",
		},
	}

	for name, testCase := range collectors {
		t.Run(name, func(t *testing.T) {
			description := collectorDescription(t, testCase.collector)
			if !strings.Contains(description, testCase.labels) {
				t.Fatalf("expected %q, got %s", testCase.labels, description)
			}
			labelStart := strings.Index(description, "variableLabels:")
			labelDescription := description[labelStart:]
			for _, forbidden := range []string{
				"knowledgebase",
				"knowledge_base",
				"client",
				"path",
				"url",
				"source",
			} {
				if strings.Contains(labelDescription, forbidden) {
					t.Errorf("metric descriptor contains unbounded label %q: %s", forbidden, description)
				}
			}
		})
	}
}

func collectorDescription(t *testing.T, collector prometheus.Collector) string {
	t.Helper()
	descriptions := make(chan *prometheus.Desc, 4)
	collector.Describe(descriptions)
	close(descriptions)

	var rendered []string
	for description := range descriptions {
		rendered = append(rendered, description.String())
	}
	if len(rendered) == 0 {
		t.Fatal("collector returned no descriptors")
	}
	return strings.Join(rendered, "\n")
}
