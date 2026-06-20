package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

const (
	finalizer               = "rag.furkan.dev/finalizer"
	ownerKey                = ".metadata.controller"
	ingestFailureRetryDelay = 5 * time.Minute
)

// KnowledgeBaseReconciler reconciles a KnowledgeBase object.
type KnowledgeBaseReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=rag.furkan.dev,resources=knowledgebases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=knowledgebases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=knowledgebases/finalizers,verbs=update
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=vectorindices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=ingestionruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=ingestionruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile drives the actual knowledge state toward the desired spec.
func (r *KnowledgeBaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var kb ragv1alpha1.KnowledgeBase
	if err := r.Get(ctx, req.NamespacedName, &kb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion path.
	if !kb.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &kb)
	}

	if !controllerutil.ContainsFinalizer(&kb, finalizer) {
		controllerutil.AddFinalizer(&kb, finalizer)
		if err := r.Update(ctx, &kb); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Keep an owned VectorIndex in sync with the store/model.
	if err := r.ensureVectorIndex(ctx, &kb); err != nil {
		return ctrl.Result{}, err
	}

	// Suspended: stop creating work, reflect state, requeue lazily.
	if kb.Spec.Suspend {
		if kb.Status.Phase != ragv1alpha1.PhaseSuspended {
			kb.Status.Phase = ragv1alpha1.PhaseSuspended
			setCondition(&kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse, "Suspended", "reconciliation suspended")
			if err := r.statusUpdate(ctx, &kb); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	secretsHash := r.computeSecretsHash(ctx, &kb)
	chash := corpusHash(&kb)

	// Track credential changes. `ObservedSecretsHash` is initialised
	// on first reconcile and updated on every reconcile when the
	// currently-observed credentials differ. The new hash is
	// persisted even if no ingestion runs, so the next Spec/Status
	// write carries the freshest fingerprint (e.g. for a UI column
	// or audit log). Secret rotation does NOT trigger re-index by
	// itself — that path is gated on ObservedSpecHash via
	// `needsIngest`.
	if kb.Status.ObservedSecretsHash != secretsHash {
		kb.Status.ObservedSecretsHash = secretsHash
	}

	// If the user edited the corpus spec while an auto-tuned chunking override is
	// active, drop the override so the new ingestion honours the spec.
	if userEditedSpec(&kb, chash) {
		recordAutoTuneDuration(&kb, "reset", time.Now())
		kb.Status.EffectiveChunking = nil
		kb.Status.AutoTuneAttempts = 0
		kb.Status.BestChunking = nil
		kb.Status.BestRecallPercent = 0
		kb.Status.PendingRetune = false
	}
	eff := effectiveChunking(&kb)

	// Finalize any in-flight job before deciding new work.
	// Cancel a running ingest if either the corpus spec or the credentials changed.
	if kb.Status.ActiveJob != "" && kb.Status.ActiveJob != kb.Name+"-cleanup" {
		var active batchv1.Job
		key := types.NamespacedName{Namespace: kb.Namespace, Name: kb.Status.ActiveJob}
		if err := r.Get(ctx, key, &active); err == nil {
			if activeIngestIsStale(&active, chash, secretsHash, kb.Status.ObservedSpecHash, kb.Status.ObservedSecretsHash) {
				logger.Info("cancelling stale ingest job due to spec or secret change", "job", active.Name)
				r.event(&kb, corev1.EventTypeNormal, "IngestionCancelled",
					"spec or secrets changed; cancelling in-flight ingest %s", active.Name)
				_ = r.Delete(ctx, &active, client.PropagationPolicy(metav1.DeletePropagationBackground))
				kb.Status.ActiveJob = ""
				kb.Status.ActiveJobStartedAt = nil
				if err := r.statusUpdate(ctx, &kb); err != nil {
					return ctrl.Result{}, err
				}
			} else if isActiveJobTimedOut(&active, kb.Status.ActiveJobStartedAt, time.Now()) {
				// Job exists, spec/creds unchanged, but it's been running
				// longer than its deadline allows. Operator likely lost
				// a watch event (restart, leader handoff). Treat as
				// failed and let the next reconcile start a new Job.
				logger.Info("active job timed out (likely lost watch event)", "job", active.Name,
					"started", kb.Status.ActiveJobStartedAt)
				r.event(&kb, corev1.EventTypeWarning, "IngestionStuck",
					"active job %s has been running past ActiveDeadlineSeconds; clearing and retrying", active.Name)
				_ = r.Delete(ctx, &active, client.PropagationPolicy(metav1.DeletePropagationBackground))
				kb.Status.ActiveJob = ""
				kb.Status.ActiveJobStartedAt = nil
				kb.Status.LastFailedSpecHash = chash
				now := metav1.Now()
				kb.Status.LastFailureTime = &now
				kb.Status.Phase = ragv1alpha1.PhaseFailed
				setCondition(&kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse,
					"IngestionStuck", "active job exceeded ActiveDeadlineSeconds; cleared")
				if err := r.statusUpdate(ctx, &kb); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
	}
	if kb.Status.ActiveJob != "" {
		res, handled, err := r.reconcileActiveJob(ctx, &kb, eff, chash, secretsHash)
		if err != nil || handled {
			return res, err
		}
	}
	if retryAfter := ingestFailureRetryAfter(&kb, chash, time.Now()); retryAfter > 0 {
		return ctrl.Result{RequeueAfter: retryAfter}, nil
	}

	// 1) Ingestion takes priority over evaluation.
	if reason, need := needsIngest(&kb, chash); need {
		return r.startIngest(ctx, &kb, eff, chash, secretsHash, reason)
	}

	// 2) Retrieval-quality evaluation.
	if evalDue(&kb) {
		return r.startEval(ctx, &kb, eff, chash, secretsHash)
	}

	// 3) Steady state: requeue near the next scheduled freshness/eval fire.
	logger.V(1).Info("steady state", "knowledgebase", kb.Name, "phase", kb.Status.Phase)
	now := time.Now()
	next := requeueFor(nextFire(kb.Spec.Freshness.Schedule, now), nextEvalFire(&kb, now))
	return ctrl.Result{RequeueAfter: next}, nil
}

func (r *KnowledgeBaseReconciler) statusUpdate(ctx context.Context, kb *ragv1alpha1.KnowledgeBase) error {
	desired := kb.Status.DeepCopy()
	key := client.ObjectKeyFromObject(kb)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest ragv1alpha1.KnowledgeBase
		if err := r.Get(ctx, key, &latest); err != nil {
			return err
		}
		latest.Status = *desired.DeepCopy()
		if err := r.Status().Update(ctx, &latest); err != nil {
			return err
		}
		kb.ResourceVersion = latest.ResourceVersion
		return nil
	})
}

func (r *KnowledgeBaseReconciler) event(obj runtime.Object, etype, reason, msg string, args ...any) {
	if r.Recorder == nil {
		return
	}
	// New events API: regarding, related, eventtype, reason, action, note.
	r.Recorder.Eventf(obj, nil, etype, reason, reason, msg, args...)
}

func (r *KnowledgeBaseReconciler) deleteJob(ctx context.Context, job *batchv1.Job) {
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil &&
		!apierrors.IsNotFound(err) {
		log.FromContext(ctx).Error(err, "failed to delete finished job", "job", job.Name)
	}
}

// SetupWithManager wires the reconciler, owned-resource watches and the Job index.
func (r *KnowledgeBaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &batchv1.Job{}, ownerKey,
		func(obj client.Object) []string {
			owner := metav1.GetControllerOf(obj)
			if owner == nil || owner.Kind != "KnowledgeBase" {
				return nil
			}
			return []string{owner.Name}
		}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&ragv1alpha1.KnowledgeBase{}).
		Owns(&batchv1.Job{}).
		Owns(&ragv1alpha1.VectorIndex{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				secret, ok := obj.(*corev1.Secret)
				if !ok {
					return nil
				}
				var list ragv1alpha1.KnowledgeBaseList
				if err := r.List(ctx, &list, client.InNamespace(secret.Namespace)); err != nil {
					return nil
				}
				var reqs []reconcile.Request
				for _, kb := range list.Items {
					referenced := false
					if kb.Spec.VectorStore.CredentialsSecretRef != nil && kb.Spec.VectorStore.CredentialsSecretRef.Name == secret.Name {
						referenced = true
					}
					if kb.Spec.Embedding.APIKeySecretRef != nil && kb.Spec.Embedding.APIKeySecretRef.Name == secret.Name {
						referenced = true
					}
					for _, s := range kb.Spec.Sources {
						if s.GitHub != nil && s.GitHub.TokenSecretRef != nil && s.GitHub.TokenSecretRef.Name == secret.Name {
							referenced = true
						}
						if s.S3 != nil {
							if s.S3.AccessKeySecretRef != nil && s.S3.AccessKeySecretRef.Name == secret.Name {
								referenced = true
							}
							if s.S3.SecretKeySecretRef != nil && s.S3.SecretKeySecretRef.Name == secret.Name {
								referenced = true
							}
						}
					}
					if referenced {
						reqs = append(reqs, reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: kb.Namespace,
								Name:      kb.Name,
							},
						})
					}
				}
				return reqs
			}),
		).
		Complete(r)
}
