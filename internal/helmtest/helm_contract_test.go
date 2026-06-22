//go:build helm

package helmtest

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestDefaultChartContract(t *testing.T) {
	objects := renderChart(t, "--namespace", "kuberag-system")

	deployment := requireObject(t, objects, "Deployment", "test-kuberag", "kuberag-system")
	assertNestedEqual(t, deployment, "ghcr.io/furkandogmus/kuberag:latest",
		"spec", "template", "spec", "containers", "0", "image")
	assertNestedEqual(t, deployment, true,
		"spec", "template", "spec", "securityContext", "runAsNonRoot")
	assertNestedEqual(t, deployment, "RuntimeDefault",
		"spec", "template", "spec", "securityContext", "seccompProfile", "type")
	assertNestedEqual(t, deployment, false,
		"spec", "template", "spec", "containers", "0", "securityContext", "allowPrivilegeEscalation")
	assertNestedEqual(t, deployment, true,
		"spec", "template", "spec", "containers", "0", "securityContext", "readOnlyRootFilesystem")

	requireObject(t, objects, "ClusterRole", "test-kuberag-role", "")
	requireObject(t, objects, "ClusterRoleBinding", "test-kuberag-rolebinding", "")
	requireObject(t, objects, "PodDisruptionBudget", "test-kuberag", "kuberag-system")
	requireObject(t, objects, "PriorityClass", "kuberag-system", "")

	for _, object := range objects {
		switch object.GetKind() {
		case "NetworkPolicy", "ServiceMonitor", "PrometheusRule":
			t.Fatalf("%s %q must be opt-in with default values", object.GetKind(), object.GetName())
		}
	}
}

func TestNamespaceScopedProductionOptions(t *testing.T) {
	objects := renderChart(t,
		"--namespace", "operator-ns",
		"--set", "rbac.scope=namespace",
		"--set", "rbac.watchNamespace=tenant-a",
		"--set", "networkPolicy.enabled=true",
		"--set", "worker.namespaces={tenant-a,tenant-b}",
		"--set", "metrics.serviceMonitor.enabled=true",
		"--set", "metrics.retrieverServiceMonitor.enabled=true",
		"--set", "metrics.prometheusRule.enabled=true",
		"--set", "image.repository=registry.example/kuberag",
		"--set", "image.tag=v1.2.3",
	)

	deployment := requireObject(t, objects, "Deployment", "test-kuberag", "operator-ns")
	assertNestedEqual(t, deployment, "registry.example/kuberag:v1.2.3",
		"spec", "template", "spec", "containers", "0", "image")
	assertContainerEnv(t, deployment, "manager", "WATCH_NAMESPACE", "tenant-a")

	role := requireObject(t, objects, "Role", "test-kuberag-role", "tenant-a")
	assertRuleContains(t, role, "rag.furkan.dev", "knowledgebases", "watch")
	binding := requireObject(t, objects, "RoleBinding", "test-kuberag-rolebinding", "tenant-a")
	assertNestedEqual(t, binding, "Role", "roleRef", "kind")
	assertNestedEqual(t, binding, "operator-ns", "subjects", "0", "namespace")

	requireObject(t, objects, "NetworkPolicy", "test-kuberag", "operator-ns")
	requireObject(t, objects, "NetworkPolicy", "test-kuberag-worker", "tenant-a")
	requireObject(t, objects, "NetworkPolicy", "test-kuberag-worker", "tenant-b")
	requireObject(t, objects, "ServiceMonitor", "test-kuberag", "operator-ns")
	retrieverMonitor := requireObject(t, objects, "ServiceMonitor", "test-kuberag-retrievers", "operator-ns")
	assertNestedEqual(t, retrieverMonitor, true, "spec", "namespaceSelector", "any")
	assertNestedEqual(t, retrieverMonitor, "retriever-metrics",
		"spec", "selector", "matchLabels", "app.kubernetes.io/component")
	rules := requireObject(t, objects, "PrometheusRule", "test-kuberag", "operator-ns")
	alerts := collectAlerts(t, rules)
	for _, alert := range []string{
		"KuberagRetrieverHighErrorRate",
		"KuberagRetrieverHighP99Latency",
		"KuberagRetrieverSaturated",
		"KuberagRetrieverRejectingTraffic",
		"KuberagControllerReconcileErrors",
		"KuberagIngestionStale",
	} {
		if !alerts[alert] {
			t.Errorf("PrometheusRule missing alert %q", alert)
		}
	}
}

func renderChart(t *testing.T, args ...string) []*unstructured.Unstructured {
	t.Helper()
	commandArgs := []string{"template", "test", "../../deploy/helm/kuberag"}
	commandArgs = append(commandArgs, args...)
	command := exec.Command("helm", commandArgs...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, output)
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var objects []*unstructured.Unstructured
	for {
		raw := map[string]any{}
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode rendered manifest: %v", err)
		}
		if len(raw) != 0 {
			objects = append(objects, &unstructured.Unstructured{Object: raw})
		}
	}
	return objects
}

func requireObject(
	t *testing.T,
	objects []*unstructured.Unstructured,
	kind, name, namespace string,
) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name && object.GetNamespace() == namespace {
			return object
		}
	}
	t.Fatalf("rendered chart missing %s %s/%s", kind, namespace, name)
	return nil
}

func nestedValue(object map[string]any, fields ...string) (any, bool) {
	current := any(object)
	for _, field := range fields {
		switch typed := current.(type) {
		case map[string]any:
			var found bool
			current, found = typed[field]
			if !found {
				return nil, false
			}
		case []any:
			index := 0
			for _, character := range field {
				if character < '0' || character > '9' {
					return nil, false
				}
				index = index*10 + int(character-'0')
			}
			if index >= len(typed) {
				return nil, false
			}
			current = typed[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func assertNestedEqual(t *testing.T, object *unstructured.Unstructured, expected any, fields ...string) {
	t.Helper()
	actual, found := nestedValue(object.Object, fields...)
	if !found || actual != expected {
		t.Errorf("%s %q field %s: expected %#v, got %#v (found=%t)",
			object.GetKind(), object.GetName(), strings.Join(fields, "."), expected, actual, found)
	}
}

func assertContainerEnv(
	t *testing.T,
	deployment *unstructured.Unstructured,
	containerName, envName, expected string,
) {
	t.Helper()
	containers, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		t.Fatalf("Deployment containers missing: %v", err)
	}
	for _, rawContainer := range containers {
		container := rawContainer.(map[string]any)
		if container["name"] != containerName {
			continue
		}
		for _, rawEnv := range container["env"].([]any) {
			env := rawEnv.(map[string]any)
			if env["name"] == envName && env["value"] == expected {
				return
			}
		}
	}
	t.Errorf("container %q missing env %s=%q", containerName, envName, expected)
}

func assertRuleContains(
	t *testing.T,
	role *unstructured.Unstructured,
	apiGroup, resource, verb string,
) {
	t.Helper()
	rules, found, err := unstructured.NestedSlice(role.Object, "rules")
	if err != nil || !found {
		t.Fatalf("Role rules missing: %v", err)
	}
	for _, rawRule := range rules {
		rule := rawRule.(map[string]any)
		if stringSliceContains(rule["apiGroups"], apiGroup) &&
			stringSliceContains(rule["resources"], resource) &&
			stringSliceContains(rule["verbs"], verb) {
			return
		}
	}
	t.Errorf("Role missing apiGroup=%q resource=%q verb=%q", apiGroup, resource, verb)
}

func stringSliceContains(value any, wanted string) bool {
	values, ok := value.([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func collectAlerts(t *testing.T, rules *unstructured.Unstructured) map[string]bool {
	t.Helper()
	groups, found, err := unstructured.NestedSlice(rules.Object, "spec", "groups")
	if err != nil || !found {
		t.Fatalf("PrometheusRule groups missing: %v", err)
	}
	alerts := map[string]bool{}
	for _, rawGroup := range groups {
		group := rawGroup.(map[string]any)
		for _, rawRule := range group["rules"].([]any) {
			rule := rawRule.(map[string]any)
			if alert, ok := rule["alert"].(string); ok {
				alerts[alert] = true
			}
		}
	}
	return alerts
}
