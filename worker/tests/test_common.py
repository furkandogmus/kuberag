import os
import sys
import types
import unittest
from unittest.mock import MagicMock, patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from rag_worker.common import write_result


class TestWriteResult(unittest.TestCase):
    def test_replaces_operator_precreated_configmap(self):
        api = MagicMock()
        client = types.SimpleNamespace(
            CoreV1Api=lambda: api,
            V1ConfigMap=lambda **kwargs: types.SimpleNamespace(**kwargs),
            V1ObjectMeta=lambda **kwargs: types.SimpleNamespace(**kwargs),
            ApiException=type("ApiException", (Exception,), {}),
        )
        config = types.SimpleNamespace(
            load_incluster_config=lambda: None,
            load_kube_config=lambda: None,
            ConfigException=type("ConfigException", (Exception,), {}),
        )
        kubernetes = types.SimpleNamespace(client=client, config=config)

        with (
            patch.dict(
                sys.modules,
                {
                    "kubernetes": kubernetes,
                    "kubernetes.client": client,
                    "kubernetes.config": config,
                },
            ),
            patch.dict(
                os.environ,
                {"RESULT_CONFIGMAP": "job-result", "KB_NAMESPACE": "tenant-a"},
                clear=False,
            ),
        ):
            write_result({"totalChunks": 3})

        api.replace_namespaced_config_map.assert_called_once()
        api.create_namespaced_config_map.assert_not_called()


if __name__ == "__main__":
    unittest.main()
