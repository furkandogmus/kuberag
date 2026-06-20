//go:build integration

// Integration tests run the reconcilers against a real kube-apiserver provided
// by envtest. Enable with: go test -tags=integration ./internal/controller/...
// Requires envtest assets (KUBEBUILDER_ASSETS); see the Makefile `test-integration`.
package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

var (
	k8sClient client.Client
	testEnv   *envtest.Environment
	testCtx   context.Context
	cancel    context.CancelFunc
)

func TestMain(m *testing.M) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Printf("failed to start envtest (is KUBEBUILDER_ASSETS set?): %v\n", err)
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = ragv1alpha1.AddToScheme(scheme)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		fmt.Printf("manager: %v\n", err)
		os.Exit(1)
	}
	mustSetup(mgr, &KnowledgeBaseReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorder("kb")})
	mustSetup(mgr, &RetrieverReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()})
	mustSetup(mgr, &VectorIndexReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()})

	testCtx, cancel = context.WithCancel(context.Background())
	go func() { _ = mgr.Start(testCtx) }()
	if !mgr.GetCache().WaitForCacheSync(testCtx) {
		fmt.Println("cache did not sync")
		os.Exit(1)
	}
	k8sClient = mgr.GetClient()

	code := m.Run()
	cancel()
	_ = testEnv.Stop()
	os.Exit(code)
}

type setupper interface{ SetupWithManager(ctrl.Manager) error }

func mustSetup(mgr ctrl.Manager, r setupper) {
	if err := r.SetupWithManager(mgr); err != nil {
		panic(err)
	}
}

// eventually polls fn until it returns nil or the timeout elapses.
func eventually(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %v", timeout, last)
}

func newNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "it-"}}
	if err := k8sClient.Create(testCtx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}

func sampleKB(ns, name string) *ragv1alpha1.KnowledgeBase {
	return &ragv1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: ragv1alpha1.KnowledgeBaseSpec{
			Sources: []ragv1alpha1.Source{{
				Name: "docs", Type: ragv1alpha1.SourceGitHub,
				GitHub: &ragv1alpha1.GitHubSource{Repo: "org/docs"},
			}},
			Embedding:   ragv1alpha1.EmbeddingSpec{Model: "bge-small", Provider: "local"},
			VectorStore: ragv1alpha1.VectorStoreSpec{Type: ragv1alpha1.VectorStoreQdrant, Endpoint: "http://qdrant:6333"},
		},
	}
}

func TestWebSourceAdmissionValidation(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "invalid-web-source")
	kb.Spec.Sources = []ragv1alpha1.Source{{
		Name: "site",
		Type: ragv1alpha1.SourceWeb,
		Web: &ragv1alpha1.WebSource{
			URLs:     []string{"ftp://example.com/docs"},
			MaxPages: -1,
		},
	}}

	err := k8sClient.Create(testCtx, kb)
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected invalid web source to be rejected by CRD validation, got %v", err)
	}
}

// completeJob marks a Job complete and writes the worker result ConfigMap the
// operator reads (envtest has no job controller, so we drive this ourselves).
func completeJob(t *testing.T, ns, jobName, resultJSON string) {
	t.Helper()
	var job batchv1.Job
	if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: jobName}, &job); err != nil {
		t.Fatalf("get job %s: %v", jobName, err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: jobName + "-result"},
		Data:       map[string]string{"result.json": resultJSON},
	}
	if err := k8sClient.Create(testCtx, cm); apierrors.IsAlreadyExists(err) {
		var existing corev1.ConfigMap
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(cm), &existing); err != nil {
			t.Fatalf("get pre-created result cm: %v", err)
		}
		existing.Data = cm.Data
		if err := k8sClient.Update(testCtx, &existing); err != nil {
			t.Fatalf("update result cm: %v", err)
		}
	} else if err != nil {
		t.Fatalf("create result cm: %v", err)
	}
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Succeeded = 1
	// k8s >=1.28 requires SuccessCriteriaMet before Complete may be set.
	job.Status.Conditions = append(job.Status.Conditions,
		batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
		batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	)
	if err := k8sClient.Status().Update(testCtx, &job); err != nil {
		t.Fatalf("update job status: %v", err)
	}
}

func firstIngestJob(ns, kb string) (*batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := k8sClient.List(testCtx, &jobs, client.InNamespace(ns),
		client.MatchingLabels{labelKB: kb, labelJobType: jobTypeIngest}); err != nil {
		return nil, err
	}
	if len(jobs.Items) == 0 {
		return nil, fmt.Errorf("no ingest job yet")
	}
	return &jobs.Items[0], nil
}

func TestKnowledgeBaseReconcileLifecycle(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "lifecycle")
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	// A VectorIndex is created for the KB.
	eventually(t, 15*time.Second, func() error {
		var vi ragv1alpha1.VectorIndex
		return k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "lifecycle-index"}, &vi)
	})

	// An ingest Job is created and the KB reports Ingesting.
	var jobName string
	eventually(t, 15*time.Second, func() error {
		job, err := firstIngestJob(ns, "lifecycle")
		if err != nil {
			return err
		}
		jobName = job.Name
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		if got.Status.Phase != ragv1alpha1.PhaseIngesting {
			return fmt.Errorf("phase=%s, want Ingesting", got.Status.Phase)
		}
		if job.Spec.Template.Spec.ServiceAccountName != "lifecycle-worker" {
			return fmt.Errorf("worker ServiceAccount=%q, want lifecycle-worker", job.Spec.Template.Spec.ServiceAccountName)
		}
		var sa corev1.ServiceAccount
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "lifecycle-worker"}, &sa); err != nil {
			return err
		}
		var role rbacv1.Role
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "lifecycle-worker"}, &role); err != nil {
			return err
		}
		if len(role.Rules) != 1 ||
			len(role.Rules[0].ResourceNames) != 1 ||
			role.Rules[0].ResourceNames[0] != resultConfigMapName(job.Name) {
			return fmt.Errorf("worker Role is not result-scoped: %#v", role.Rules)
		}
		var binding rbacv1.RoleBinding
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "lifecycle-worker"}, &binding); err != nil {
			return err
		}
		return nil
	})

	eventually(t, 15*time.Second, func() error {
		var run ragv1alpha1.IngestionRun
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: jobName}, &run); err != nil {
			return err
		}
		if run.Status.Phase != ragv1alpha1.IngestionRunRunning {
			return fmt.Errorf("ingestion run phase=%s, want Running", run.Status.Phase)
		}
		if run.Status.StartTime == nil {
			return fmt.Errorf("ingestion run startTime not set")
		}
		return nil
	})

	// Drive the job to completion and publish a result.
	completeJob(t, ns, jobName, `{"totalChunks":42,"sources":[{"name":"docs","revision":"abc123","chunks":42}]}`)

	// The KB becomes Ready with the indexed chunk count recorded.
	eventually(t, 20*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		if got.Status.Phase != ragv1alpha1.PhaseReady {
			return fmt.Errorf("phase=%s, want Ready", got.Status.Phase)
		}
		if got.Status.IndexedChunks != 42 {
			return fmt.Errorf("indexedChunks=%d, want 42", got.Status.IndexedChunks)
		}
		if got.Status.ObservedSpecHash == "" {
			return fmt.Errorf("observedSpecHash not set")
		}
		return nil
	})

	eventually(t, 15*time.Second, func() error {
		var run ragv1alpha1.IngestionRun
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: jobName}, &run); err != nil {
			return err
		}
		if run.Status.Phase != ragv1alpha1.IngestionRunSucceeded {
			return fmt.Errorf("ingestion run phase=%s, want Succeeded", run.Status.Phase)
		}
		if run.Status.TotalChunks != 42 {
			return fmt.Errorf("ingestion run totalChunks=%d, want 42", run.Status.TotalChunks)
		}
		return nil
	})

	eventually(t, 15*time.Second, func() error {
		var job batchv1.Job
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: jobName}, &job)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("completed ingestion job still present (err=%v)", err)
	})
	eventually(t, 15*time.Second, func() error {
		var role rbacv1.Role
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "lifecycle-worker"}, &role); err != nil {
			return err
		}
		if len(role.Rules) != 0 {
			return fmt.Errorf("worker Role still has access after Job completion: %#v", role.Rules)
		}
		return nil
	})

	// A spec edit starts one replacement ingest and keeps it alive across the
	// Job-created reconcile even though ObservedSpecHash still names the last
	// completed spec.
	if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), kb); err != nil {
		t.Fatalf("refresh kb before spec edit: %v", err)
	}
	kb.Spec.Chunking.MaxTokens++
	if err := k8sClient.Update(testCtx, kb); err != nil {
		t.Fatalf("update kb chunking: %v", err)
	}
	var replacementName string
	eventually(t, 15*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		if got.Status.ActiveJob == "" {
			return fmt.Errorf("replacement ingest not started")
		}
		replacementName = got.Status.ActiveJob
		var replacement batchv1.Job
		return k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: replacementName}, &replacement)
	})
	time.Sleep(500 * time.Millisecond)
	var got ragv1alpha1.KnowledgeBase
	if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
		t.Fatalf("read kb after replacement reconcile: %v", err)
	}
	if got.Status.ActiveJob != replacementName {
		t.Fatalf("replacement ingest churned: activeJob=%q, want %q", got.Status.ActiveJob, replacementName)
	}
	var replacement batchv1.Job
	if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: replacementName}, &replacement); err != nil {
		t.Fatalf("replacement ingest was deleted: %v", err)
	}
}

func TestKnowledgeBaseWaitsForCustomWorkerServiceAccount(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "custom-worker")
	kb.Spec.Ingestion.ServiceAccountName = "irsa-worker"
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	eventually(t, 15*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, ragv1alpha1.ConditionReady)
		if got.Status.Phase != ragv1alpha1.PhasePending ||
			cond == nil || cond.Reason != "WorkerServiceAccountUnavailable" {
			return fmt.Errorf("phase=%s condition=%v", got.Status.Phase, cond)
		}
		if _, err := firstIngestJob(ns, kb.Name); err == nil {
			return fmt.Errorf("ingest Job should not exist before custom ServiceAccount")
		}
		return nil
	})

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "irsa-worker", Namespace: ns}}
	if err := k8sClient.Create(testCtx, sa); err != nil {
		t.Fatalf("create custom ServiceAccount: %v", err)
	}
	eventually(t, 40*time.Second, func() error {
		job, err := firstIngestJob(ns, kb.Name)
		if err != nil {
			return err
		}
		if job.Spec.Template.Spec.ServiceAccountName != "irsa-worker" {
			return fmt.Errorf("job ServiceAccount=%q", job.Spec.Template.Spec.ServiceAccountName)
		}
		return nil
	})
}

func TestFailedIngestionWaitsAndSpecChangeAdvancesToUniqueRetryName(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "failed-ingest")
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	var job batchv1.Job
	eventually(t, 15*time.Second, func() error {
		found, err := firstIngestJob(ns, kb.Name)
		if err != nil {
			return err
		}
		job = *found
		return nil
	})

	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{
			Type:   batchv1.JobFailureTarget,
			Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded",
		},
		{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded",
		},
	}
	if err := k8sClient.Status().Update(testCtx, &job); err != nil {
		t.Fatalf("mark job failed: %v", err)
	}
	previousName := job.Name
	eventually(t, 15*time.Second, func() error {
		var run ragv1alpha1.IngestionRun
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: previousName}, &run); err != nil {
			return err
		}
		if run.Status.Phase != ragv1alpha1.IngestionRunFailed {
			return fmt.Errorf("ingestion run phase=%s, want Failed", run.Status.Phase)
		}
		return nil
	})

	eventually(t, 15*time.Second, func() error {
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), kb); err != nil {
			return err
		}
		if kb.Status.Phase != ragv1alpha1.PhaseFailed {
			return fmt.Errorf("phase=%s, want Failed", kb.Status.Phase)
		}
		if kb.Status.ActiveJob != "" {
			return fmt.Errorf("failed ingestion retried immediately as %q", kb.Status.ActiveJob)
		}
		if kb.Status.LastFailureTime == nil || kb.Status.LastFailedSpecHash == "" {
			return fmt.Errorf("failure cooldown status not recorded")
		}
		return nil
	})
	time.Sleep(500 * time.Millisecond)
	if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), kb); err != nil {
		t.Fatalf("read failed kb: %v", err)
	}
	if kb.Status.ActiveJob != "" {
		t.Fatalf("failed ingestion entered a hot retry loop with %q", kb.Status.ActiveJob)
	}

	kb.Spec.Chunking.MaxTokens++
	if err := k8sClient.Update(testCtx, kb); err != nil {
		t.Fatalf("change failed ingestion spec: %v", err)
	}
	eventually(t, 15*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		if got.Status.ActiveJob == "" || got.Status.ActiveJob == previousName {
			return fmt.Errorf("retry job not advanced: activeJob=%q", got.Status.ActiveJob)
		}
		var retry batchv1.Job
		return k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: got.Status.ActiveJob}, &retry)
	})
}

func TestKnowledgeBaseFinalizerCleanup(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "cleanup")
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}
	// Wait until the finalizer is attached.
	eventually(t, 15*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		if len(got.Finalizers) == 0 {
			return fmt.Errorf("no finalizer yet")
		}
		return nil
	})

	if err := k8sClient.Delete(testCtx, kb); err != nil {
		t.Fatalf("delete kb: %v", err)
	}

	// A cleanup Job is created; complete it so the finalizer releases.
	eventually(t, 15*time.Second, func() error {
		var job batchv1.Job
		return k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "cleanup-cleanup"}, &job)
	})
	completeJob(t, ns, "cleanup-cleanup", `{}`)

	// The KnowledgeBase is fully removed.
	eventually(t, 20*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("kb still present (err=%v)", err)
	})
}

func TestRetrieverCreatesServingWorkload(t *testing.T) {
	ns := newNamespace(t)
	if err := k8sClient.Create(testCtx, sampleKB(ns, "served")); err != nil {
		t.Fatalf("create kb: %v", err)
	}
	for name, data := range map[string]struct {
		key   string
		value string
	}{
		"oidc-client-id":     {"clientID", "kuberag"},
		"oidc-client-secret": {"clientSecret", "client-secret"},
		"oidc-cookie":        {"cookieSecret", "MDEyMzQ1Njc4OWFiY2RlZg=="},
	} {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       map[string][]byte{data.key: []byte(data.value)},
		}
		if err := k8sClient.Create(testCtx, secret); err != nil {
			t.Fatalf("create OIDC secret %s: %v", name, err)
		}
	}
	rt := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "served", Namespace: ns},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "served"},
			Replicas:         2,
			RateLimit: &ragv1alpha1.RateLimitSpec{
				Enabled:           boolPtr(true),
				RequestsPerMinute: 120,
				Burst:             30,
			},
			Autoscaling: &ragv1alpha1.AutoscalingSpec{
				Enabled:                        boolPtr(true),
				MinReplicas:                    2,
				MaxReplicas:                    5,
				TargetCPUUtilizationPercentage: 65,
			},
			Ingress: &ragv1alpha1.RetrieverIngressSpec{
				Host:          "rag.example.com",
				ClassName:     "nginx",
				TLSSecretName: "rag-example-tls",
				ClusterIssuer: "letsencrypt-prod",
				Annotations:   map[string]string{"nginx.ingress.kubernetes.io/proxy-read-timeout": "120"},
			},
			OIDC: &ragv1alpha1.OIDCSpec{
				IssuerURL:             "https://id.example.com",
				ClientIDSecretRef:     ragv1alpha1.SecretKeyRef{Name: "oidc-client-id", Key: "clientID"},
				ClientSecretSecretRef: ragv1alpha1.SecretKeyRef{Name: "oidc-client-secret", Key: "clientSecret"},
				CookieSecretRef:       ragv1alpha1.SecretKeyRef{Name: "oidc-cookie", Key: "cookieSecret"},
				EmailDomains:          []string{"example.com"},
			},
		},
	}
	if err := k8sClient.Create(testCtx, rt); err != nil {
		t.Fatalf("create retriever: %v", err)
	}

	eventually(t, 15*time.Second, func() error {
		var dep appsv1.Deployment
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "served-retriever"}, &dep); err != nil {
			return err
		}
		var svc corev1.Service
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "served-retriever"}, &svc); err != nil {
			return err
		}
		if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].TargetPort.IntVal != 4180 {
			return fmt.Errorf("service does not target oauth2-proxy: %#v", svc.Spec.Ports)
		}
		if len(dep.Spec.Template.Spec.Containers) != 2 || dep.Spec.Template.Spec.Containers[1].Name != "oauth2-proxy" {
			return fmt.Errorf("oauth2-proxy sidecar missing: %#v", dep.Spec.Template.Spec.Containers)
		}
		var ingress networkingv1.Ingress
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "served-retriever"}, &ingress); err != nil {
			return err
		}
		if ingress.Spec.Rules[0].Host != "rag.example.com" ||
			ingress.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt-prod" ||
			len(ingress.Spec.TLS) != 1 || ingress.Spec.TLS[0].SecretName != "rag-example-tls" {
			return fmt.Errorf("unexpected ingress: %#v", ingress)
		}
		var networkPolicy networkingv1.NetworkPolicy
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "served-retriever-oidc"}, &networkPolicy); err != nil {
			return err
		}
		if len(networkPolicy.Spec.Ingress) != 1 ||
			len(networkPolicy.Spec.Ingress[0].Ports) != 1 ||
			networkPolicy.Spec.Ingress[0].Ports[0].Port == nil ||
			networkPolicy.Spec.Ingress[0].Ports[0].Port.IntVal != 4180 {
			return fmt.Errorf("OIDC network policy does not isolate the upstream: %#v", networkPolicy.Spec)
		}
		var pdb policyv1.PodDisruptionBudget
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "served-retriever"}, &pdb); err != nil {
			return err
		}
		var hpa autoscalingv2.HorizontalPodAutoscaler
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "served-retriever"}, &hpa); err != nil {
			return err
		}
		if hpa.Spec.MaxReplicas != 5 || hpa.Spec.MinReplicas == nil || *hpa.Spec.MinReplicas != 2 {
			return fmt.Errorf("unexpected HPA bounds: min=%v max=%d", hpa.Spec.MinReplicas, hpa.Spec.MaxReplicas)
		}
		env := dep.Spec.Template.Spec.Containers[0].Env
		for _, item := range env {
			if item.Name == "RATE_LIMIT_REQUESTS_PER_MINUTE" && item.Value == "120" {
				return nil
			}
		}
		return fmt.Errorf("rate-limit env not found")
	})
}

func TestRetrieverWaitsForReferencedSecret(t *testing.T) {
	ns := newNamespace(t)
	if err := k8sClient.Create(testCtx, sampleKB(ns, "secret-gated")); err != nil {
		t.Fatalf("create kb: %v", err)
	}
	rt := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-gated", Namespace: ns},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "secret-gated"},
			Replicas:         1,
			APIKeySecretRef:  &ragv1alpha1.SecretKeyRef{Name: "retriever-auth", Key: "apiKey"},
		},
	}
	if err := k8sClient.Create(testCtx, rt); err != nil {
		t.Fatalf("create retriever: %v", err)
	}

	eventually(t, 15*time.Second, func() error {
		var got ragv1alpha1.Retriever
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(rt), &got); err != nil {
			return err
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, ragv1alpha1.ConditionAvailable)
		if got.Status.Phase != "Pending" || cond == nil || cond.Reason != "SecretNotFound" {
			return fmt.Errorf("phase=%s condition=%v", got.Status.Phase, cond)
		}
		var dep appsv1.Deployment
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "secret-gated-retriever"}, &dep)
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("deployment should not exist while Secret is missing: %v", err)
		}
		return nil
	})

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "retriever-auth", Namespace: ns},
		Data:       map[string][]byte{"apiKey": []byte("secret-value")},
	}
	if err := k8sClient.Create(testCtx, secret); err != nil {
		t.Fatalf("create auth secret: %v", err)
	}
	eventually(t, 15*time.Second, func() error {
		var dep appsv1.Deployment
		return k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "secret-gated-retriever"}, &dep)
	})

	if err := k8sClient.Delete(testCtx, secret); err != nil {
		t.Fatalf("delete auth secret: %v", err)
	}
	eventually(t, 15*time.Second, func() error {
		var dep appsv1.Deployment
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "secret-gated-retriever"}, &dep)
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("deployment should be removed after Secret deletion: %v", err)
		}
		var got ragv1alpha1.Retriever
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(rt), &got); err != nil {
			return err
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, ragv1alpha1.ConditionAvailable)
		if cond == nil || cond.Reason != "SecretNotFound" {
			return fmt.Errorf("unexpected condition after Secret deletion: %v", cond)
		}
		return nil
	})
}

// TestKnowledgeBaseAutoTuneLoop drives the full eval -> tune -> re-index ->
// re-eval -> exhaustion loop against the real apiserver. It also guards the
// re-index Job-name collision: envtest has no Job TTL controller, so if the
// re-index reused the completed initial-ingest Job name the loop would stall
// here (its result ConfigMap is already gone), failing the test.
func TestKnowledgeBaseAutoTuneLoop(t *testing.T) {
	ns := newNamespace(t)
	on := true
	kb := sampleKB(ns, "autotune")
	kb.Spec.RetrievalQuality = &ragv1alpha1.RetrievalQualitySpec{
		Enabled:              &on,
		DatasetRef:           ragv1alpha1.LocalObjectRef{Name: "eval-ds"},
		MinimumRecallPercent: 90,
		AutoTune:             &ragv1alpha1.AutoTuneSpec{Enabled: &on, MaxAttempts: 1},
	}
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	// waitActiveJob blocks until status.activeJob points at a Job of the given
	// type and returns its name.
	waitActiveJob := func(jobType string) string {
		var name string
		eventually(t, 20*time.Second, func() error {
			var got ragv1alpha1.KnowledgeBase
			if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
				return err
			}
			if got.Status.ActiveJob == "" {
				return fmt.Errorf("no active job yet (phase=%s)", got.Status.Phase)
			}
			var job batchv1.Job
			if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: got.Status.ActiveJob}, &job); err != nil {
				return err
			}
			if job.Labels[labelJobType] != jobType {
				return fmt.Errorf("active job type=%s, want %s", job.Labels[labelJobType], jobType)
			}
			name = got.Status.ActiveJob
			return nil
		})
		return name
	}

	// 1. Initial ingestion.
	ingest0 := waitActiveJob(jobTypeIngest)
	completeJob(t, ns, ingest0, `{"totalChunks":10,"sources":[{"name":"docs","revision":"r0","chunks":10}]}`)

	// 2. First evaluation comes back below the 90% target.
	eval1 := waitActiveJob(jobTypeEval)
	completeJob(t, ns, eval1, `{"recallPercent":50,"queries":4}`)

	// 3. Auto-tune fires: one attempt recorded and chunking actually tuned
	//    (default overlap 80 grows).
	eventually(t, 20*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		if got.Status.AutoTuneAttempts != 1 {
			return fmt.Errorf("attempts=%d, want 1", got.Status.AutoTuneAttempts)
		}
		if got.Status.EffectiveChunking == nil || got.Status.EffectiveChunking.Overlap <= 80 {
			return fmt.Errorf("effective chunking not tuned: %+v", got.Status.EffectiveChunking)
		}
		return nil
	})

	// 4. A fresh re-index Job is created (distinct from the completed initial one).
	reindex := waitActiveJob(jobTypeIngest)
	if reindex == ingest0 {
		t.Fatalf("re-index reused stale ingest Job name %q — auto-tune would stall", ingest0)
	}
	completeJob(t, ns, reindex, `{"totalChunks":12,"sources":[{"name":"docs","revision":"r1","chunks":12}]}`)

	// 5. Re-evaluation still below target; a new eval round runs.
	eval2 := waitActiveJob(jobTypeEval)
	if eval2 == eval1 {
		t.Fatalf("eval round did not advance (%q reused)", eval1)
	}
	completeJob(t, ns, eval2, `{"recallPercent":55,"queries":4}`)

	// 6. Attempts exhausted (maxAttempts=1) -> Degraded.
	eventually(t, 20*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		if got.Status.Phase != ragv1alpha1.PhaseDegraded {
			return fmt.Errorf("phase=%s, want Degraded", got.Status.Phase)
		}
		return nil
	})
}

// TestKnowledgeBaseAutoTuneRevertsToBest drives a *regressing* recall sequence so
// the operator must, on exhaustion, re-index back to the best config it saw
// (here the original default chunking) rather than settling on the worse last
// ladder step. It also guards that the revert re-index does not collide on Job
// name with the prior attempt (same AutoTuneAttempts, different chunking).
func TestKnowledgeBaseAutoTuneRevertsToBest(t *testing.T) {
	ns := newNamespace(t)
	on := true
	kb := sampleKB(ns, "revert")
	kb.Spec.RetrievalQuality = &ragv1alpha1.RetrievalQualitySpec{
		Enabled:              &on,
		DatasetRef:           ragv1alpha1.LocalObjectRef{Name: "eval-ds"},
		MinimumRecallPercent: 90,
		AutoTune:             &ragv1alpha1.AutoTuneSpec{Enabled: &on, MaxAttempts: 2},
	}
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	waitActiveJob := func(jobType string) string {
		var name string
		eventually(t, 20*time.Second, func() error {
			var got ragv1alpha1.KnowledgeBase
			if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
				return err
			}
			if got.Status.ActiveJob == "" {
				return fmt.Errorf("no active job yet (phase=%s)", got.Status.Phase)
			}
			var job batchv1.Job
			if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: got.Status.ActiveJob}, &job); err != nil {
				return err
			}
			if job.Labels[labelJobType] != jobType {
				return fmt.Errorf("active job type=%s, want %s", job.Labels[labelJobType], jobType)
			}
			name = got.Status.ActiveJob
			return nil
		})
		return name
	}

	// Initial ingest, then a sequence of evaluations whose recall *drops*: the
	// best config is therefore the very first (default) chunking.
	completeJob(t, ns, waitActiveJob(jobTypeIngest), `{"totalChunks":10,"sources":[{"name":"docs","revision":"r0","chunks":10}]}`)
	completeJob(t, ns, waitActiveJob(jobTypeEval), `{"recallPercent":60,"queries":5}`) // best (default chunking)
	completeJob(t, ns, waitActiveJob(jobTypeIngest), `{"totalChunks":11,"sources":[{"name":"docs","revision":"r1","chunks":11}]}`)
	completeJob(t, ns, waitActiveJob(jobTypeEval), `{"recallPercent":40,"queries":5}`) // attempt 1: worse
	completeJob(t, ns, waitActiveJob(jobTypeIngest), `{"totalChunks":12,"sources":[{"name":"docs","revision":"r2","chunks":12}]}`)
	completeJob(t, ns, waitActiveJob(jobTypeEval), `{"recallPercent":30,"queries":5}`) // attempt 2: worse still, exhausted

	// Exhausted: the operator reverts to best, which forces one more re-index whose
	// Job name must NOT collide with attempt 2 (same attempt counter, but the
	// chunk fingerprint differs).
	revert := waitActiveJob(jobTypeIngest)
	completeJob(t, ns, revert, `{"totalChunks":10,"sources":[{"name":"docs","revision":"r3","chunks":10}]}`)
	// Re-eval on the reverted (best) config, still below target -> terminal Degraded.
	completeJob(t, ns, waitActiveJob(jobTypeEval), `{"recallPercent":60,"queries":5}`)

	eventually(t, 20*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		if got.Status.Phase != ragv1alpha1.PhaseDegraded {
			return fmt.Errorf("phase=%s, want Degraded", got.Status.Phase)
		}
		if got.Status.BestRecallPercent != 60 {
			return fmt.Errorf("bestRecallPercent=%d, want 60", got.Status.BestRecallPercent)
		}
		// Landed back on the original default chunking (the best config seen).
		ec := got.Status.EffectiveChunking
		if ec == nil || ec.Strategy != ragv1alpha1.ChunkSemantic || ec.MaxTokens != 800 || ec.Overlap != 80 {
			return fmt.Errorf("did not revert to best (default) chunking: %+v", ec)
		}
		return nil
	})
}

// TestKnowledgeBaseEvalEmptyDataset verifies that an evaluation over an empty
// dataset (queries=0) does not trip the recall gate: no auto-tune, no Degraded,
// just a NoDataset condition. Otherwise a meaningless recall 0% would churn the
// auto-tune loop pointlessly.
func TestKnowledgeBaseEvalEmptyDataset(t *testing.T) {
	ns := newNamespace(t)
	on := true
	kb := sampleKB(ns, "nodata")
	kb.Spec.RetrievalQuality = &ragv1alpha1.RetrievalQualitySpec{
		Enabled:              &on,
		DatasetRef:           ragv1alpha1.LocalObjectRef{Name: "eval-ds"},
		MinimumRecallPercent: 90,
		AutoTune:             &ragv1alpha1.AutoTuneSpec{Enabled: &on, MaxAttempts: 3},
	}
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	waitActiveJob := func(jobType string) string {
		var name string
		eventually(t, 20*time.Second, func() error {
			var got ragv1alpha1.KnowledgeBase
			if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
				return err
			}
			if got.Status.ActiveJob == "" {
				return fmt.Errorf("no active job yet (phase=%s)", got.Status.Phase)
			}
			var job batchv1.Job
			if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: got.Status.ActiveJob}, &job); err != nil {
				return err
			}
			if job.Labels[labelJobType] != jobType {
				return fmt.Errorf("active job type=%s, want %s", job.Labels[labelJobType], jobType)
			}
			name = got.Status.ActiveJob
			return nil
		})
		return name
	}

	completeJob(t, ns, waitActiveJob(jobTypeIngest), `{"totalChunks":8,"sources":[{"name":"docs","revision":"r0","chunks":8}]}`)
	// Evaluation comes back empty: recall 0 over 0 queries.
	completeJob(t, ns, waitActiveJob(jobTypeEval), `{"recallPercent":0,"queries":0}`)

	eventually(t, 20*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		if got.Status.ActiveJob != "" {
			return fmt.Errorf("still has active job %q", got.Status.ActiveJob)
		}
		// Must NOT have auto-tuned or degraded on an empty dataset.
		if got.Status.AutoTuneAttempts != 0 {
			return fmt.Errorf("auto-tune fired on empty dataset: attempts=%d", got.Status.AutoTuneAttempts)
		}
		if got.Status.Phase == ragv1alpha1.PhaseDegraded {
			return fmt.Errorf("phase Degraded on empty dataset")
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, ragv1alpha1.ConditionEvaluated)
		if cond == nil || cond.Reason != "NoDataset" {
			return fmt.Errorf("Evaluated condition reason=%v, want NoDataset", cond)
		}
		return nil
	})
}

func TestCRDValidations(t *testing.T) {
	ns := newNamespace(t)

	// 1. Test overlap >= maxTokens validation
	kb1 := sampleKB(ns, "invalid-overlap")
	kb1.Spec.Chunking = ragv1alpha1.ChunkingSpec{
		Strategy:  ragv1alpha1.ChunkFixed,
		MaxTokens: 100,
		Overlap:   100, // Invalid: overlap must be < maxTokens
	}
	err := k8sClient.Create(testCtx, kb1)
	if err == nil {
		t.Error("expected creation of KnowledgeBase with overlap >= maxTokens to fail")
	}

	// 2. Test duplicate source names validation
	kb2 := sampleKB(ns, "duplicate-sources")
	kb2.Spec.Sources = []ragv1alpha1.Source{
		{Name: "src1", Type: ragv1alpha1.SourceGitHub, GitHub: &ragv1alpha1.GitHubSource{Repo: "org/docs"}},
		{Name: "src1", Type: ragv1alpha1.SourceGitHub, GitHub: &ragv1alpha1.GitHubSource{Repo: "org/wiki"}},
	}
	err = k8sClient.Create(testCtx, kb2)
	if err == nil {
		t.Error("expected creation of KnowledgeBase with duplicate source names to fail")
	}

	// 3. Test openai-compatible embedding without baseURL validation
	kb3 := sampleKB(ns, "missing-baseurl-embedding")
	kb3.Spec.Embedding = ragv1alpha1.EmbeddingSpec{
		Model:    "custom-model",
		Provider: "openai-compatible",
		// BaseURL: "", // Invalid: required when provider is openai-compatible
	}
	err = k8sClient.Create(testCtx, kb3)
	if err == nil {
		t.Error("expected creation of KnowledgeBase with openai-compatible embedding but missing baseURL to fail")
	}

	// 4. Test openai-compatible generation without baseURL validation
	rt := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-baseurl-gen", Namespace: ns},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "served"},
			Replicas:         1,
			Generation: &ragv1alpha1.GenerationSpec{
				Enabled:  boolPtr(true),
				Provider: "openai-compatible",
				Model:    "custom-chat-model",
				// BaseURL: "", // Invalid: required when provider is openai-compatible
			},
		},
	}
	err = k8sClient.Create(testCtx, rt)
	if err == nil {
		t.Error("expected creation of Retriever with openai-compatible generation but missing baseURL to fail")
	}

	// 5. Test invalid HPA bounds.
	rt2 := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-hpa", Namespace: ns},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "served"},
			Replicas:         1,
			Autoscaling: &ragv1alpha1.AutoscalingSpec{
				Enabled:     boolPtr(true),
				MinReplicas: 5,
				MaxReplicas: 2,
			},
		},
	}
	err = k8sClient.Create(testCtx, rt2)
	if err == nil {
		t.Error("expected creation of Retriever with maxReplicas < minReplicas to fail")
	}

	// 6. OIDC requires an Ingress and cannot be combined with native API-key auth.
	oidc := &ragv1alpha1.OIDCSpec{
		IssuerURL:             "https://id.example.com",
		ClientIDSecretRef:     ragv1alpha1.SecretKeyRef{Name: "oidc", Key: "clientID"},
		ClientSecretSecretRef: ragv1alpha1.SecretKeyRef{Name: "oidc", Key: "clientSecret"},
		CookieSecretRef:       ragv1alpha1.SecretKeyRef{Name: "oidc", Key: "cookieSecret"},
	}
	rt3 := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "oidc-without-ingress", Namespace: ns},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "served"},
			Replicas:         1,
			OIDC:             oidc,
		},
	}
	if err := k8sClient.Create(testCtx, rt3); err == nil {
		t.Error("expected OIDC Retriever without ingress to fail admission")
	}

	rt4 := rt3.DeepCopy()
	rt4.Name = "oidc-with-api-key"
	rt4.Spec.Ingress = &ragv1alpha1.RetrieverIngressSpec{Host: "rag.example.com"}
	rt4.Spec.APIKeySecretRef = &ragv1alpha1.SecretKeyRef{Name: "auth", Key: "apiKey"}
	if err := k8sClient.Create(testCtx, rt4); err == nil {
		t.Error("expected OIDC Retriever with apiKeySecretRef to fail admission")
	}
}

func TestCRDPruning(t *testing.T) {
	ns := newNamespace(t)
	name := "prune-test"

	unstructuredKB := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rag.furkan.dev/v1alpha1",
			"kind":       "KnowledgeBase",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"sources": []interface{}{
					map[string]interface{}{
						"name":   "docs",
						"type":   "github",
						"github": map[string]interface{}{"repo": "org/docs"},
					},
				},
				"embedding": map[string]interface{}{
					"model":    "bge-small",
					"provider": "local",
				},
				"vectorStore": map[string]interface{}{
					"type":     "qdrant",
					"endpoint": "http://qdrant:6333",
				},
				"chunkingg": map[string]interface{}{
					"maxTokens": 999,
				},
			},
		},
	}

	err := k8sClient.Create(testCtx, unstructuredKB)
	if err != nil {
		t.Fatalf("create KB with typo field: %v", err)
	}

	var created ragv1alpha1.KnowledgeBase
	if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: name}, &created); err != nil {
		t.Fatalf("read back created KB: %v", err)
	}
	if created.Spec.Chunking.MaxTokens == 999 {
		t.Error("chunkingg field was not pruned; Chunking.MaxTokens unexpectedly set to 999")
	}

	eventually(t, 30*time.Second, func() error {
		var kb ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: name}, &kb); err != nil {
			return err
		}
		if kb.Status.Phase == "" || kb.Status.Phase == ragv1alpha1.PhasePending {
			return fmt.Errorf("still pending")
		}
		return nil
	})
}
