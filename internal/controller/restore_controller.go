package controller

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

const restoreFinalizer = "rag.furkan.dev/restore"

// RestoreReconciler manages the lifecycle of Restore resources.
type RestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=rag.furkan.dev,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=restores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=restores/finalizers,verbs=update
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=backups,verbs=get;list;watch
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=knowledgebases,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *RestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var rest ragv1alpha1.Restore
	if err := r.Get(ctx, req.NamespacedName, &rest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tracer := otel.Tracer("kuberag")
	ctx, span := tracer.Start(ctx, "Restore.Reconcile",
		trace.WithAttributes(
			attribute.String("restore.name", rest.Name),
			attribute.String("restore.namespace", rest.Namespace),
		))
	defer span.End()

	if rest.Spec.Suspend {
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&rest, restoreFinalizer) {
		controllerutil.AddFinalizer(&rest, restoreFinalizer)
		if err := r.Update(ctx, &rest); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if !rest.DeletionTimestamp.IsZero() {
		controllerutil.RemoveFinalizer(&rest, restoreFinalizer)
		return ctrl.Result{}, r.Update(ctx, &rest)
	}

	if rest.Status.Phase == ragv1alpha1.RestorePhaseCompleted || rest.Status.Phase == ragv1alpha1.RestorePhaseFailed {
		return ctrl.Result{}, nil
	}

	var backup ragv1alpha1.Backup
	backupKey := types.NamespacedName{Namespace: rest.Namespace, Name: rest.Spec.BackupRef.Name}
	if err := r.Get(ctx, backupKey, &backup); err != nil {
		if apierrors.IsNotFound(err) {
			rest.Status.Phase = ragv1alpha1.RestorePhaseFailed
			setRestoreCond(&rest, metav1.ConditionFalse, "BackupNotFound", "referenced Backup not found")
			return ctrl.Result{}, r.Status().Update(ctx, &rest)
		}
		return ctrl.Result{}, err
	}

	if backup.Status.Phase != ragv1alpha1.BackupPhaseCompleted || backup.Status.Location == "" {
		rest.Status.Phase = ragv1alpha1.RestorePhaseFailed
		setRestoreCond(&rest, metav1.ConditionFalse, "BackupNotReady", "referenced Backup is not in Completed phase")
		return ctrl.Result{}, r.Status().Update(ctx, &rest)
	}

	var kb ragv1alpha1.KnowledgeBase
	kbKey := types.NamespacedName{Namespace: rest.Namespace, Name: rest.Spec.KnowledgeBaseRef.Name}
	if err := r.Get(ctx, kbKey, &kb); err != nil {
		if apierrors.IsNotFound(err) {
			rest.Status.Phase = ragv1alpha1.RestorePhaseFailed
			setRestoreCond(&rest, metav1.ConditionFalse, "KnowledgeBaseNotFound", "referenced KnowledgeBase not found")
			return ctrl.Result{}, r.Status().Update(ctx, &rest)
		}
		return ctrl.Result{}, err
	}

	if rest.Status.ActiveJob == "" {
		job, specJSON, err := buildRestoreJob(ctx, &kb, &rest, &backup)
		if err != nil {
			return ctrl.Result{}, err
		}
		cm := specConfigMap(rest.Namespace, specConfigMapName(job.Name), specJSON)
		if cerr := r.Create(ctx, cm); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			if isQuotaExceeded(cerr) {
				rest.Status.Phase = ragv1alpha1.RestorePhaseFailed
				setRestoreCond(&rest, metav1.ConditionFalse, "ResourceQuotaExceeded",
					"cannot create ConfigMap: resource quota exceeded")
				return ctrl.Result{}, r.Status().Update(ctx, &rest)
			}
			return ctrl.Result{}, cerr
		}
		if cerr := r.Create(ctx, job); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			if isQuotaExceeded(cerr) {
				rest.Status.Phase = ragv1alpha1.RestorePhaseFailed
				setRestoreCond(&rest, metav1.ConditionFalse, "ResourceQuotaExceeded",
					"cannot create Job: resource quota exceeded")
				return ctrl.Result{}, r.Status().Update(ctx, &rest)
			}
			return ctrl.Result{}, cerr
		}
		rest.Status.Phase = ragv1alpha1.RestorePhaseRunning
		rest.Status.ActiveJob = job.Name
		setRestoreCond(&rest, metav1.ConditionFalse, "RestoreJobCreated", "restore job started")
		return ctrl.Result{}, r.Status().Update(ctx, &rest)
	}

	var job batchv1.Job
	jobKey := types.NamespacedName{Namespace: rest.Namespace, Name: rest.Status.ActiveJob}
	if err := r.Get(ctx, jobKey, &job); err != nil {
		if apierrors.IsNotFound(err) {
			rest.Status.ActiveJob = ""
			return ctrl.Result{Requeue: true}, r.Status().Update(ctx, &rest)
		}
		return ctrl.Result{}, err
	}

	if !jobComplete(&job) && !jobFailed(&job) {
		logger.Info("restore job still running", "job", job.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if jobFailed(&job) {
		rest.Status.Phase = ragv1alpha1.RestorePhaseFailed
		setRestoreCond(&rest, metav1.ConditionFalse, "RestoreFailed", "restore job failed")
		return ctrl.Result{}, r.Status().Update(ctx, &rest)
	}

	var result RestoreResult
	if err := readJobResult(ctx, r.Client, rest.Namespace, job.Name, &result); err != nil {
		rest.Status.Phase = ragv1alpha1.RestorePhaseFailed
		setRestoreCond(&rest, metav1.ConditionFalse, "ResultReadFailed", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, &rest)
	}

	now := metav1.Now()
	rest.Status.Phase = ragv1alpha1.RestorePhaseCompleted
	rest.Status.CompletionTime = &now
	rest.Status.RestoredPoints = result.RestoredPoints
	setRestoreCond(&rest, metav1.ConditionTrue, "RestoreComplete", "restore completed successfully")
	deleteResultCM(ctx, r.Client, rest.Namespace, job.Name)
	deleteSpecCM(ctx, r.Client, rest.Namespace, job.Name)
	return ctrl.Result{}, r.Status().Update(ctx, &rest)
}

func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ragv1alpha1.Restore{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

func setRestoreCond(rest *ragv1alpha1.Restore, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&rest.Status.Conditions, metav1.Condition{
		Type:               ragv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}
