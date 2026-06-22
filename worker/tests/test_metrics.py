import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from retriever.metrics import RetrieverMetrics


class TestRetrieverMetrics(unittest.TestCase):
    def test_prometheus_histogram_and_rejection_output(self):
        registry = RetrieverMetrics()
        registry.query_started()
        registry.query_finished("success", 0.2)
        registry.rejected("rate_limit")

        output = registry.render().decode()
        self.assertIn(
            'kuberag_retriever_queries_total{result="success"} 1',
            output,
        )
        self.assertIn(
            'kuberag_retriever_query_duration_seconds_bucket{result="success",le="0.25"} 1',
            output,
        )
        self.assertIn(
            'kuberag_retriever_rejected_requests_total{reason="rate_limit"} 1',
            output,
        )
        self.assertIn("kuberag_retriever_queries_in_flight 0", output)

    def test_build_and_capacity_metrics(self):
        registry = RetrieverMetrics()
        registry.set_capacity(32)
        body = registry.render().decode()
        self.assertIn("kuberag_retriever_concurrency_limit 32", body)
        self.assertIn("kuberag_retriever_build_info", body)

    def test_dynamic_labels_are_collapsed_to_bounded_other_value(self):
        registry = RetrieverMetrics()
        for value in ("tenant-a", "tenant-b", "10.0.0.1"):
            registry.query_started()
            registry.query_finished(value, 0.01)
            registry.rejected(value)

        output = registry.render().decode()
        self.assertIn(
            'kuberag_retriever_queries_total{result="other"} 3',
            output,
        )
        self.assertIn(
            'kuberag_retriever_rejected_requests_total{reason="other"} 3',
            output,
        )
        self.assertNotIn("tenant-a", output)
        self.assertNotIn("tenant-b", output)
        self.assertNotIn("10.0.0.1", output)


if __name__ == "__main__":
    unittest.main()
