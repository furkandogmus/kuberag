import json
import unittest
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[2]


class TestObservabilityAssets(unittest.TestCase):
    def test_only_one_canonical_grafana_dashboard_exists(self):
        self.assertTrue(
            (ROOT / "config/observability/grafana-dashboard.json").is_file()
        )
        self.assertFalse(
            (ROOT / "docs/grafana-slo-dashboard.json").exists()
        )

    def test_prometheus_rule_contains_retriever_slos(self):
        document = yaml.safe_load(
            (ROOT / "config/observability/prometheusrule.yaml").read_text()
        )
        alerts = {
            rule["alert"]
            for group in document["spec"]["groups"]
            for rule in group["rules"]
        }
        self.assertTrue({
            "KuberagRetrieverHighErrorRate",
            "KuberagRetrieverHighP99Latency",
            "KuberagRetrieverSaturated",
            "KuberagRetrieverRejectingTraffic",
            "KuberagControllerReconcileErrors",
            "KuberagIngestionStale",
        }.issubset(alerts))

    def test_retriever_service_monitor_selects_metrics_services(self):
        document = yaml.safe_load(
            (ROOT / "config/observability/retriever-servicemonitor.yaml").read_text()
        )
        self.assertTrue(document["spec"]["namespaceSelector"]["any"])
        self.assertEqual(
            document["spec"]["selector"]["matchLabels"]["app.kubernetes.io/component"],
            "retriever-metrics",
        )
        self.assertEqual(document["spec"]["endpoints"][0]["port"], "metrics")

    def test_dashboard_uses_bounded_namespace_and_retriever_metrics(self):
        dashboard = json.loads(
            (ROOT / "config/observability/grafana-dashboard.json").read_text()
        )
        variables = {item["name"] for item in dashboard["templating"]["list"]}
        self.assertIn("namespace", variables)
        expressions = " ".join(
            target["expr"]
            for panel in dashboard["panels"]
            for target in panel.get("targets", [])
        )
        self.assertIn("kuberag_retriever_query_duration_seconds_bucket", expressions)
        self.assertIn("kuberag_retriever_rejected_requests_total", expressions)
        self.assertIn(
            "rag_knowledgebase_last_successful_ingestion_timestamp_seconds",
            expressions,
        )
        self.assertNotIn('knowledgebase=~"$knowledgebase"', expressions)


if __name__ == "__main__":
    unittest.main()
