import unittest
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[2]


class TestSecurityAssets(unittest.TestCase):
    def assert_restricted_contexts(self, pod_context, container_context):
        self.assertTrue(pod_context["runAsNonRoot"])
        self.assertEqual(
            pod_context["seccompProfile"]["type"],
            "RuntimeDefault",
        )
        self.assertTrue(container_context["runAsNonRoot"])
        self.assertFalse(container_context["allowPrivilegeEscalation"])
        self.assertTrue(container_context["readOnlyRootFilesystem"])
        self.assertIn("ALL", container_context["capabilities"]["drop"])
        self.assertEqual(
            container_context["seccompProfile"]["type"],
            "RuntimeDefault",
        )

    def test_helm_defaults_meet_restricted_pod_security(self):
        values = yaml.safe_load(
            (ROOT / "deploy/helm/kuberag/values.yaml").read_text()
        )
        self.assert_restricted_contexts(
            values["podSecurityContext"],
            values["securityContext"],
        )

    def test_static_manager_manifest_meets_restricted_pod_security(self):
        documents = yaml.safe_load_all(
            (ROOT / "config/manager/manager.yaml").read_text()
        )
        deployment = next(
            document for document in documents
            if document and document.get("kind") == "Deployment"
        )
        pod_spec = deployment["spec"]["template"]["spec"]
        container_context = pod_spec["containers"][0]["securityContext"]
        container_context["runAsNonRoot"] = pod_spec["securityContext"]["runAsNonRoot"]
        container_context["seccompProfile"] = pod_spec["securityContext"]["seccompProfile"]
        self.assert_restricted_contexts(
            pod_spec["securityContext"],
            container_context,
        )

    def test_image_policy_enforces_release_workflow_signatures(self):
        policy = yaml.safe_load(
            (
                ROOT / "config/security/kyverno-verify-images.yaml"
            ).read_text()
        )
        self.assertEqual(policy["apiVersion"], "policies.kyverno.io/v1")
        self.assertEqual(policy["kind"], "ImageValidatingPolicy")
        spec = policy["spec"]
        self.assertEqual(spec["validationActions"], ["Deny"])
        self.assertEqual(spec["failurePolicy"], "Fail")
        self.assertEqual(
            spec["matchImageReferences"][0]["glob"],
            "ghcr.io/furkandogmus/kuberag*",
        )
        self.assertEqual(
            spec["validationConfigurations"],
            {
                "mutateDigest": True,
                "required": True,
                "verifyDigest": True,
            },
        )
        keyless = spec["attestors"][0]["cosign"]["keyless"]["identities"][0]
        self.assertIn(
            "github\\.com/furkandogmus/kuberag",
            keyless["subjectRegExp"],
        )
        self.assertIn("release\\.yaml", keyless["subjectRegExp"])
        self.assertEqual(
            keyless["issuer"],
            "https://token.actions.githubusercontent.com",
        )
        ctlog = spec["attestors"][0]["cosign"]["ctlog"]
        self.assertEqual(ctlog["url"], "https://rekor.sigstore.dev")
        self.assertFalse(ctlog["insecureIgnoreTlog"])
        self.assertFalse(ctlog["insecureIgnoreSCT"])

    def test_production_reference_enforces_tenant_baseline(self):
        documents = [
            document
            for document in yaml.safe_load_all(
                (
                    ROOT / "config/samples/production-reference.yaml"
                ).read_text()
            )
            if document
        ]
        resources = {
            (document["kind"], document["metadata"]["name"]): document
            for document in documents
        }
        namespace = resources[("Namespace", "tenant-prod")]
        labels = namespace["metadata"]["labels"]
        self.assertEqual(
            labels["pod-security.kubernetes.io/enforce"],
            "restricted",
        )
        self.assertIn(("ResourceQuota", "kuberag-workloads"), resources)
        self.assertIn(("LimitRange", "kuberag-defaults"), resources)
        kb = resources[("KnowledgeBase", "company-docs")]
        self.assertNotIn("latest", kb["spec"]["workerImage"])
        retriever = resources[("Retriever", "company-docs")]
        self.assertNotIn("latest", retriever["spec"]["image"])
        self.assertTrue(retriever["spec"]["oidc"])
        self.assertTrue(retriever["spec"]["ingress"]["tlsSecretName"])


if __name__ == "__main__":
    unittest.main()
