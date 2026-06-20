package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

// reconcileDelete runs a cleanup Job to drop the remote collection, then frees the finalizer.
func (r *KnowledgeBaseReconciler) reconcileDelete(ctx context.Context, kb *ragv1alpha1.KnowledgeBase) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(kb, finalizer) {
		return ctrl.Result{}, nil
	}

	cleanupName := truncName(fmt.Sprintf("%s-cleanup", kb.Name))
	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Namespace: kb.Namespace, Name: cleanupName}, &job)
	switch {
	case apierrors.IsNotFound(err):
		secretsHash := r.computeSecretsHash(ctx, kb)
		cj, specJSON, berr := buildCleanupJob(ctx, kb, secretsHash)
		if berr != nil {
			return ctrl.Result{}, berr
		}
		cm := specConfigMap(kb.Namespace, specConfigMapName(cj.Name), specJSON)
		if cerr := r.Create(ctx, cm); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return ctrl.Result{}, cerr
		}
		// No controller ref: the KB is being deleted, so the Job must outlive it.
		if cerr := r.Create(ctx, cj); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return ctrl.Result{}, cerr
		}
		r.event(kb, corev1.EventTypeNormal, "Cleanup", "dropping vector store collection")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	case err != nil:
		return ctrl.Result{}, err
	}

	if !jobComplete(&job) && !jobFailed(&job) {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	// Only release the finalizer when cleanup actually succeeded.
	// On failure, delete the failed Job and retry — the finalizer keeps the
	// KB alive so the operator gets another chance to drop the collection.
	if jobFailed(&job) {
		r.event(kb, corev1.EventTypeWarning, "CleanupFailed",
			"cleanup job %s failed; retrying", job.Name)
		_ = r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	// Cleanup succeeded. Remove the cleanup Job and finalizer.
	_ = r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	controllerutil.RemoveFinalizer(kb, finalizer)
	if err := r.Update(ctx, kb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
