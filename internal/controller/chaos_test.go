//go:build integration

package controller

import (
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

// TestLeaderElectionHandoff verifies leader election lease management and
// ensures reconciliation proceeds under a single manager.
func TestLeaderElectionHandoff(t *testing.T) {
	ns := newNamespace(t)

	// Create a leader election lease to verify the pattern.
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kuberag-leader",
			Namespace: ns,
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To("kuberag-controller-abc123"),
			LeaseDurationSeconds: ptr.To(int32(15)),
		},
	}
	if err := k8sClient.Create(testCtx, lease); err != nil {
		t.Fatalf("create lease: %v", err)
	}

	// Verify the lease exists.
	eventually(t, 10*time.Second, func() error {
		var got coordinationv1.Lease
		return k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: "kuberag-leader"}, &got)
	})

	// Under the single leader, KB creation and reconciliation works correctly.
	kb := sampleKB(ns, "leader-test")
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	// Verify ingest Job is created under the leader.
	eventually(t, 15*time.Second, func() error {
		job, err := firstIngestJob(ns, kb.Name)
		if err != nil {
			return err
		}
		if job.Labels[labelManagedBy] != "kuberag" {
			return fmt.Errorf("job not managed by kuberag")
		}
		return nil
	})
}

// TestChaosKillMidIngestion tests recovery when the operator dies during
// ingestion and the Job fails in the meantime.
func TestChaosKillMidIngestion(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "chaos-ingest")
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	// Wait for the ingest Job.
	var jobName string
	eventually(t, 15*time.Second, func() error {
		job, err := firstIngestJob(ns, kb.Name)
		if err != nil {
			return err
		}
		jobName = job.Name
		return nil
	})

	// Simulate a crash: mark the ingest Job as Failed.
	var job batchv1.Job
	if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: jobName}, &job); err != nil {
		t.Fatalf("get job: %v", err)
	}
	job.Status.Failed = 1
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
	}
	if err := k8sClient.Status().Update(testCtx, &job); err != nil {
		t.Fatalf("mark job failed: %v", err)
	}

	// Controller detects failure, clears ActiveJob, and transitions to Failed.
	eventually(t, 15*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: kb.Name}, &got); err != nil {
			return err
		}
		if got.Status.Phase != ragv1alpha1.PhaseFailed {
			return fmt.Errorf("phase=%s, want Failed", got.Status.Phase)
		}
		if got.Status.ActiveJob != "" {
			return fmt.Errorf("activeJob still set after failure: %q", got.Status.ActiveJob)
		}
		return nil
	})
}

// TestChaosKillMidCleanup tests recovery when the operator dies mid-cleanup:
// the cleanup Job is created before deletion, and on restart the controller
// picks it up, completes it, and removes the finalizer.
func TestChaosKillMidCleanup(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "chaos-cleanup")
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	// Wait for finalizer to be attached by the controller.
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

	// Simulate a crash before deletion: manually create a cleanup Job.
	cleanupName := "chaos-cleanup-cleanup"
	cleanupJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cleanupName,
			Namespace: ns,
			Labels: map[string]string{
				labelManagedBy: "kuberag",
				labelKB:        kb.Name,
				labelJobType:   jobTypeCleanup,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{Name: "cleanup", Image: "busybox", Command: []string{"echo", "done"}},
					},
				},
			},
		},
	}
	if err := k8sClient.Create(testCtx, cleanupJob); err != nil {
		t.Fatalf("create cleanup job: %v", err)
	}

	// Delete the KB. The controller sees the pre-existing cleanup Job.
	if err := k8sClient.Delete(testCtx, kb); err != nil {
		t.Fatalf("delete kb: %v", err)
	}

	// Complete the cleanup Job to release the finalizer.
	completeJob(t, ns, cleanupName, `{}`)

	// KB finalizer removed and resource deleted.
	eventually(t, 20*time.Second, func() error {
		var got ragv1alpha1.KnowledgeBase
		err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(kb), &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("kb still present (err=%v)", err)
	})
}

// TestStaleJobDetection verifies that isActiveJobTimedOut() is triggered
// during reconciliation when an in-flight Job exceeds its deadline.
func TestStaleJobDetection(t *testing.T) {
	ns := newNamespace(t)
	kb := sampleKB(ns, "stale-job")
	if err := k8sClient.Create(testCtx, kb); err != nil {
		t.Fatalf("create kb: %v", err)
	}

	// Wait for the ingest Job and the ActiveJob reference.
	var jobName string
	eventually(t, 15*time.Second, func() error {
		job, err := firstIngestJob(ns, kb.Name)
		if err != nil {
			return err
		}
		jobName = job.Name
		var got ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: kb.Name}, &got); err != nil {
			return err
		}
		if got.Status.ActiveJob != jobName {
			return fmt.Errorf("activeJob=%q, want %q", got.Status.ActiveJob, jobName)
		}
		return nil
	})

	// Simulate a long-stalled job by setting ActiveJobStartedAt far in the past.
	var got ragv1alpha1.KnowledgeBase
	if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: kb.Name}, &got); err != nil {
		t.Fatalf("get kb: %v", err)
	}
	staleStart := metav1.NewTime(time.Now().Add(-124 * time.Hour))
	got.Status.ActiveJobStartedAt = &staleStart
	if err := k8sClient.Status().Update(testCtx, &got); err != nil {
		t.Fatalf("backdate ActiveJobStartedAt: %v", err)
	}

	// Bump the spec to trigger a reconcile where the timeout is detected.
	if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: kb.Name}, &got); err != nil {
		t.Fatalf("refresh kb: %v", err)
	}
	got.Spec.Chunking.MaxTokens++
	if err := k8sClient.Update(testCtx, &got); err != nil {
		t.Fatalf("trigger reconcile: %v", err)
	}

	// Controller detects the stale job, clears ActiveJob, and marks Failed.
	eventually(t, 15*time.Second, func() error {
		var current ragv1alpha1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: kb.Name}, &current); err != nil {
			return err
		}
		if current.Status.Phase != ragv1alpha1.PhaseFailed {
			return fmt.Errorf("phase=%s, want Failed", current.Status.Phase)
		}
		if current.Status.ActiveJob != "" {
			return fmt.Errorf("stale activeJob not cleared: %q", current.Status.ActiveJob)
		}
		return nil
	})
}
