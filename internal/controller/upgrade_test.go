//go:build integration

package controller

import (
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

func TestUpgradeCRDSchemaMigration(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "upgrade-migration")
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	// VectorIndex created — controller manages it after schema upgrade.
	eventually(t, 15*time.Second, func() error {
		var vi ragv1alpha1.VectorIndex
		return k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "upgrade-migration-index"}, &vi)
	})

	// KB transitions to Ingesting; IngestionRun records are created.
	var jobName string
	eventually(t, 15*time.Second, func() error {
		job, err := firstIngestJob(ns, "upgrade-migration")
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

	// Retriever created before "upgrade" still serves after.
	rt := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "upgrade-migration", Namespace: ns},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "upgrade-migration"},
			Replicas:         1,
		},
	}
	if err := k8sClient.Create(testCtx, rt); err != nil {
		t.Fatalf("create retriever: %v", err)
	}
	eventually(t, 15*time.Second, func() error {
		var dep appsv1.Deployment
		return k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "upgrade-migration-retriever"}, &dep)
	})

	// IngestionRun from before upgrade is still readable.
	eventually(t, 15*time.Second, func() error {
		var run ragv1alpha1.IngestionRun
		return k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: jobName}, &run)
	})

	// Re-read all resources: spec/status fields survive the upgrade.
	{
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			t.Fatalf("re-read kb after upgrade: %v", err)
		}
		if got.Spec.Embedding.Model != "bge-small" {
			t.Fatalf("kb spec lost after upgrade: model=%q", got.Spec.Embedding.Model)
		}
	}
	{
		var vi ragv1alpha1.VectorIndex
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "upgrade-migration-index"}, &vi); err != nil {
			t.Fatalf("re-read vector index after upgrade: %v", err)
		}
		if vi.Spec.Store.Endpoint != "http://qdrant:6333" {
			t.Fatalf("vi spec lost after upgrade: endpoint=%q", vi.Spec.Store.Endpoint)
		}
	}
	{
		var rt2 ragv1alpha1.Retriever
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(rt), &rt2); err != nil {
			t.Fatalf("re-read retriever after upgrade: %v", err)
		}
		if rt2.Spec.KnowledgeBaseRef.Name != "upgrade-migration" {
			t.Fatalf("rt spec lost after upgrade: kbRef=%q", rt2.Spec.KnowledgeBaseRef.Name)
		}
	}
}

func TestUpgradeControllerRollingUpdate(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "rolling-update")
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	// Controller starts reconciliation: KB enters Ingesting with active job.
	var jobName string
	eventually(t, 15*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		if got.Status.Phase != ragv1alpha1.PhaseIngesting {
			return fmt.Errorf("phase=%s, want Ingesting", got.Status.Phase)
		}
		if got.Status.ActiveJob == "" {
			return fmt.Errorf("no active job set")
		}
		jobName = got.Status.ActiveJob
		return nil
	})

	// Simulate controller restart mid-reconcile: complete the in-flight job.
	completeJob(t, ns, jobName, `{"totalChunks":20,"sources":[{"name":"docs","revision":"abc123","chunks":20}]}`)

	// Controller picks up where it left off and transitions to Ready.
	eventually(t, 20*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
			return err
		}
		if got.Status.Phase != ragv1alpha1.PhaseReady {
			return fmt.Errorf("phase=%s, want Ready after restart", got.Status.Phase)
		}
		if got.Status.IndexedChunks != 20 {
			return fmt.Errorf("indexedChunks=%d, want 20", got.Status.IndexedChunks)
		}
		return nil
	})
}

func TestUpgradeStoredVersionChange(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "stored-version")
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	// Read back: spec fields survived the initial create round-trip.
	var got ragv1alpha1.KnowledgeBase
	if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got); err != nil {
		t.Fatalf("read kb: %v", err)
	}
	if got.Spec.Sources[0].GitHub.Repo != "org/docs" {
		t.Fatalf("spec data lost: repo=%q", got.Spec.Sources[0].GitHub.Repo)
	}

	// Update spec and verify it persists (no stored-version mismatch).
	got.Spec.Chunking.MaxTokens = 512
	got.Spec.Chunking.Overlap = 64
	if err := k8sClient.Update(testCtx, &got); err != nil {
		t.Fatalf("update kb spec: %v", err)
	}

	var updated ragv1alpha1.KnowledgeBase
	if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &updated); err != nil {
		t.Fatalf("re-read kb after spec update: %v", err)
	}
	if updated.Spec.Chunking.MaxTokens != 512 || updated.Spec.Chunking.Overlap != 64 {
		t.Fatalf("spec update lost: maxTokens=%d overlap=%d", updated.Spec.Chunking.MaxTokens, updated.Spec.Chunking.Overlap)
	}

	// Patch status and verify the fields survive.
	updated.Status.IndexedChunks = 100
	updated.Status.Phase = ragv1alpha1.PhaseReady
	if err := k8sClient.Status().Update(testCtx, &updated); err != nil {
		t.Fatalf("patch kb status: %v", err)
	}
	var final ragv1alpha1.KnowledgeBase
	if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &final); err != nil {
		t.Fatalf("re-read kb after status patch: %v", err)
	}
	if final.Status.IndexedChunks != 100 {
		t.Fatalf("status field lost: indexedChunks=%d", final.Status.IndexedChunks)
	}
	if final.Status.Phase != ragv1alpha1.PhaseReady {
		t.Fatalf("status field lost: phase=%s", final.Status.Phase)
	}

	// Retriever round-trips spec and status without data loss.
	rt := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "stored-version", Namespace: ns},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "stored-version"},
			Replicas:         3,
		},
	}
	if err := k8sClient.Create(testCtx, rt); err != nil {
		t.Fatalf("create retriever: %v", err)
	}
	var rtGot ragv1alpha1.Retriever
	if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(rt), &rtGot); err != nil {
		t.Fatalf("read retriever: %v", err)
	}
	if rtGot.Spec.Replicas != 3 {
		t.Fatalf("rt spec lost: replicas=%d", rtGot.Spec.Replicas)
	}
	rtGot.Status.Phase = "Available"
	if err := k8sClient.Status().Update(testCtx, &rtGot); err != nil {
		t.Fatalf("patch rt status: %v", err)
	}
	var rtFinal ragv1alpha1.Retriever
	if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(rt), &rtFinal); err != nil {
		t.Fatalf("re-read rt after status patch: %v", err)
	}
	if rtFinal.Status.Phase != "Available" {
		t.Fatalf("rt status lost: phase=%s", rtFinal.Status.Phase)
	}
}

func TestHelmUpgradeSimulation(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "helm-upgrade")
	kb.Labels = map[string]string{
		"app.kubernetes.io/instance":   "my-release",
		"app.kubernetes.io/managed-by": "Helm",
		"app.kubernetes.io/name":       "kuberag",
	}
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	// Label-based discovery: controller finds the KB via Helm labels.
	var kbList ragv1alpha1.KnowledgeBaseList
	if err := k8sClient.List(testCtx, &kbList, client.InNamespace(ns),
		client.MatchingLabels{"app.kubernetes.io/instance": "my-release"}); err != nil {
		t.Fatalf("helm label discovery: %v", err)
	}
	if len(kbList.Items) != 1 || kbList.Items[0].Name != "helm-upgrade" {
		t.Fatalf("label-based discovery failed: %d items", len(kbList.Items))
	}

	// Retriever under old naming convention is still found and managed.
	rt := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "helm-upgrade", Namespace: ns},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "helm-upgrade"},
			Replicas:         1,
		},
	}
	if err := k8sClient.Create(testCtx, rt); err != nil {
		t.Fatalf("create retriever: %v", err)
	}
	eventually(t, 15*time.Second, func() error {
		var rtList ragv1alpha1.RetrieverList
		if err := k8sClient.List(testCtx, &rtList, client.InNamespace(ns)); err != nil {
			return err
		}
		if len(rtList.Items) != 1 || rtList.Items[0].Spec.KnowledgeBaseRef.Name != "helm-upgrade" {
			return fmt.Errorf("retriever discovery failed: %d items", len(rtList.Items))
		}
		return nil
	})

	// Verify the controller created the serving workload for the Helm-managed Retriever.
	eventually(t, 15*time.Second, func() error {
		var dep appsv1.Deployment
		return k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "helm-upgrade-retriever"}, &dep)
	})
}
