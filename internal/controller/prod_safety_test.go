package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

func prodSafetyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for name, add := range map[string]func(*runtime.Scheme) error{
		"core":  corev1.AddToScheme,
		"batch": batchv1.AddToScheme,
		"rbac":  rbacv1.AddToScheme,
		"rag":   ragv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add %s types to scheme: %v", name, err)
		}
	}
	return scheme
}

func TestMinimalWorkerSpecsExcludeLargeSources(t *testing.T) {
	kb := baseKB()
	kb.Spec.Sources[0].GitHub.IncludeGlobs = []string{strings.Repeat("x", maxConfigMapBytes)}

	querySpec, err := marshalQuerySpec(kb)
	if err != nil {
		t.Fatalf("marshal query spec: %v", err)
	}
	storeSpec, err := marshalStoreSpec(kb)
	if err != nil {
		t.Fatalf("marshal store spec: %v", err)
	}

	for name, specJSON := range map[string]string{"query": querySpec, "store": storeSpec} {
		if !specConfigMapSizeOK(specJSON) {
			t.Fatalf("%s spec should remain below the ConfigMap safety limit", name)
		}
		if strings.Contains(specJSON, `"sources"`) || strings.Contains(specJSON, strings.Repeat("x", 128)) {
			t.Fatalf("%s spec unexpectedly contains source configuration", name)
		}
	}
	if !strings.Contains(querySpec, `"embedding"`) || !strings.Contains(querySpec, `"vectorStore"`) {
		t.Fatal("query spec must contain embedding and vector store configuration")
	}
	if strings.Contains(storeSpec, `"embedding"`) || !strings.Contains(storeSpec, `"vectorStore"`) {
		t.Fatal("store spec must contain only vector store configuration")
	}
}

func TestOversizedIngestionSpecFailsWithoutCreatingResources(t *testing.T) {
	ctx := context.Background()
	scheme := prodSafetyScheme(t)
	kb := baseKB()
	kb.Generation = 7
	kb.Spec.Sources[0].GitHub.IncludeGlobs = []string{strings.Repeat("x", maxConfigMapBytes)}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ragv1alpha1.KnowledgeBase{}).
		WithObjects(kb).
		Build()
	reconciler := &KnowledgeBaseReconciler{Client: fakeClient, Scheme: scheme}

	result, err := reconciler.startIngest(
		ctx, kb, effectiveChunking(kb), corpusHash(kb), "secrets", "initial",
	)
	if err != nil {
		t.Fatalf("start ingest: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("oversized immutable spec should wait for a spec change, got %+v", result)
	}

	var latest ragv1alpha1.KnowledgeBase
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(kb), &latest); err != nil {
		t.Fatalf("get KnowledgeBase: %v", err)
	}
	if latest.Status.Phase != ragv1alpha1.PhaseFailed {
		t.Fatalf("expected failed phase, got %q", latest.Status.Phase)
	}
	condition := meta.FindStatusCondition(latest.Status.Conditions, ragv1alpha1.ConditionReady)
	if condition == nil || condition.Reason != "SpecConfigTooLarge" || condition.ObservedGeneration != kb.Generation {
		t.Fatalf("unexpected Ready condition: %+v", condition)
	}
	if !specConfigTooLargeUnchanged(&latest) {
		t.Fatal("unchanged oversized generation should suppress further ingestion attempts")
	}

	var jobs batchv1.JobList
	if err := fakeClient.List(ctx, &jobs, client.InNamespace(kb.Namespace)); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	var configMaps corev1.ConfigMapList
	if err := fakeClient.List(ctx, &configMaps, client.InNamespace(kb.Namespace)); err != nil {
		t.Fatalf("list ConfigMaps: %v", err)
	}
	if len(jobs.Items) != 0 || len(configMaps.Items) != 0 {
		t.Fatalf("oversized spec created resources: jobs=%d configmaps=%d", len(jobs.Items), len(configMaps.Items))
	}
}

func TestQuotaExceededDuringIngestionSetsConditionAndRequeues(t *testing.T) {
	ctx := context.Background()
	scheme := prodSafetyScheme(t)
	kb := baseKB()
	kb.Generation = 3

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ragv1alpha1.KnowledgeBase{}).
		WithObjects(kb).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(
				ctx context.Context,
				delegate client.WithWatch,
				obj client.Object,
				opts ...client.CreateOption,
			) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return apierrors.NewForbidden(
						schema.GroupResource{Resource: "configmaps"},
						obj.GetName(),
						errors.New("exceeded quota: namespace compute"),
					)
				}
				return delegate.Create(ctx, obj, opts...)
			},
		}).
		Build()
	reconciler := &KnowledgeBaseReconciler{Client: fakeClient, Scheme: scheme}

	result, err := reconciler.startIngest(
		ctx, kb, effectiveChunking(kb), corpusHash(kb), "secrets", "initial",
	)
	if err != nil {
		t.Fatalf("quota errors should be reflected in status, got: %v", err)
	}
	if result.RequeueAfter != time.Minute {
		t.Fatalf("expected one-minute quota retry, got %+v", result)
	}

	var latest ragv1alpha1.KnowledgeBase
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(kb), &latest); err != nil {
		t.Fatalf("get KnowledgeBase: %v", err)
	}
	condition := meta.FindStatusCondition(latest.Status.Conditions, ragv1alpha1.ConditionReady)
	if condition == nil || condition.Reason != "ResourceQuotaExceeded" {
		t.Fatalf("unexpected Ready condition: %+v", condition)
	}

	var jobs batchv1.JobList
	if err := fakeClient.List(ctx, &jobs, client.InNamespace(kb.Namespace)); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("quota rejection should prevent Job creation, got %d", len(jobs.Items))
	}
}

func TestQuotaExceededClassificationRequiresQuotaMessage(t *testing.T) {
	quotaErr := apierrors.NewForbidden(
		schema.GroupResource{Resource: "jobs"},
		"job",
		errors.New("exceeded limitrange: cpu"),
	)
	if !isQuotaExceeded(quotaErr) {
		t.Fatal("expected LimitRange rejection to be classified as quota exceeded")
	}

	rbacErr := apierrors.NewForbidden(
		schema.GroupResource{Resource: "jobs"},
		"job",
		errors.New("user cannot create jobs"),
	)
	if isQuotaExceeded(rbacErr) {
		t.Fatal("generic RBAC forbidden errors must not be classified as quota exceeded")
	}
}

func TestGeneratedWorkloadsMeetRestrictedPodSecurity(t *testing.T) {
	kb := baseKB()
	ingest, _, err := buildIngestJob(
		context.Background(), kb, "hash", "secrets", ragv1alpha1.IngestFull, effectiveChunking(kb),
	)
	if err != nil {
		t.Fatalf("build ingest Job: %v", err)
	}
	backup, _, err := buildBackupJob(context.Background(), kb, baseBackup(), "backup-id")
	if err != nil {
		t.Fatalf("build backup Job: %v", err)
	}
	completedBackup := baseBackup()
	completedBackup.Status.Location = "s3://bucket/backup.tar.gz"
	restore, _, err := buildRestoreJob(context.Background(), kb, baseRestore(), completedBackup)
	if err != nil {
		t.Fatalf("build restore Job: %v", err)
	}

	retriever := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "retriever", Namespace: "default"},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: kb.Name},
			Replicas:         1,
			Ingress:          &ragv1alpha1.RetrieverIngressSpec{Host: "rag.example.com"},
			OIDC: &ragv1alpha1.OIDCSpec{
				IssuerURL:             "https://issuer.example.com",
				ClientIDSecretRef:     ragv1alpha1.SecretKeyRef{Name: "oidc", Key: "client-id"},
				ClientSecretSecretRef: ragv1alpha1.SecretKeyRef{Name: "oidc", Key: "client-secret"},
				CookieSecretRef:       ragv1alpha1.SecretKeyRef{Name: "oidc", Key: "cookie-secret"},
			},
		},
	}
	deployment := (&RetrieverReconciler{}).desiredDeployment(retriever, kb, "secret-hash")

	workloads := map[string]corev1.PodSpec{
		"ingest":    ingest.Spec.Template.Spec,
		"backup":    backup.Spec.Template.Spec,
		"restore":   restore.Spec.Template.Spec,
		"retriever": deployment.Spec.Template.Spec,
	}
	for name, podSpec := range workloads {
		assertRestrictedPodSpec(t, name, podSpec)
	}
}

func assertRestrictedPodSpec(t *testing.T, name string, podSpec corev1.PodSpec) {
	t.Helper()
	if podSpec.SecurityContext == nil ||
		podSpec.SecurityContext.RunAsNonRoot == nil ||
		!*podSpec.SecurityContext.RunAsNonRoot {
		t.Errorf("%s: pod must set runAsNonRoot=true", name)
	}
	if podSpec.SecurityContext == nil ||
		podSpec.SecurityContext.SeccompProfile == nil ||
		podSpec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("%s: pod must use RuntimeDefault seccomp", name)
	}
	if podSpec.HostNetwork || podSpec.HostPID || podSpec.HostIPC {
		t.Errorf("%s: host namespace sharing is forbidden", name)
	}
	for _, volume := range podSpec.Volumes {
		if volume.HostPath != nil {
			t.Errorf("%s: hostPath volume %q is forbidden", name, volume.Name)
		}
	}
	for _, container := range append(append([]corev1.Container{}, podSpec.InitContainers...), podSpec.Containers...) {
		sc := container.SecurityContext
		if sc == nil ||
			sc.AllowPrivilegeEscalation == nil ||
			*sc.AllowPrivilegeEscalation {
			t.Errorf("%s/%s: allowPrivilegeEscalation must be false", name, container.Name)
		}
		if sc.Capabilities == nil || !containsCapability(sc.Capabilities.Drop, "ALL") {
			t.Errorf("%s/%s: all Linux capabilities must be dropped", name, container.Name)
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("%s/%s: root filesystem must be read-only", name, container.Name)
		}
	}
}

func containsCapability(capabilities []corev1.Capability, wanted corev1.Capability) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}
