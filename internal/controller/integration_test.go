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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
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
	if err := k8sClient.Create(testCtx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
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
	rt := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "served", Namespace: ns},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "served"},
			Replicas:         1,
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
		return k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "served-retriever"}, &svc)
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
}
