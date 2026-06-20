package controller

import (
	"context"
	"fmt"
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

const backupFinalizer = "rag.furkan.dev/backup"

// BackupReconciler manages the lifecycle of Backup resources.
type BackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=rag.furkan.dev,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=backups/finalizers,verbs=update
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=knowledgebases,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var bkp ragv1alpha1.Backup
	if err := r.Get(ctx, req.NamespacedName, &bkp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tracer := otel.Tracer("kuberag")
	ctx, span := tracer.Start(ctx, "Backup.Reconcile",
		trace.WithAttributes(
			attribute.String("backup.name", bkp.Name),
			attribute.String("backup.namespace", bkp.Namespace),
		))
	defer span.End()

	if bkp.Spec.Suspend {
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&bkp, backupFinalizer) {
		controllerutil.AddFinalizer(&bkp, backupFinalizer)
		if err := r.Update(ctx, &bkp); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if !bkp.DeletionTimestamp.IsZero() {
		controllerutil.RemoveFinalizer(&bkp, backupFinalizer)
		return ctrl.Result{}, r.Update(ctx, &bkp)
	}

	if bkp.Status.Phase == ragv1alpha1.BackupPhaseCompleted || bkp.Status.Phase == ragv1alpha1.BackupPhaseFailed {
		return ctrl.Result{}, nil
	}

	var kb ragv1alpha1.KnowledgeBase
	kbKey := types.NamespacedName{Namespace: bkp.Namespace, Name: bkp.Spec.KnowledgeBaseRef.Name}
	if err := r.Get(ctx, kbKey, &kb); err != nil {
		if apierrors.IsNotFound(err) {
			bkp.Status.Phase = ragv1alpha1.BackupPhaseFailed
			setBackupCond(&bkp, metav1.ConditionFalse, "KnowledgeBaseNotFound", "referenced KnowledgeBase not found")
			return ctrl.Result{}, r.Status().Update(ctx, &bkp)
		}
		return ctrl.Result{}, err
	}

	if bkp.Status.ActiveJob == "" {
		backupID := fmt.Sprintf("%d", time.Now().Unix())
		bkp.Status.BackupID = backupID

		job, specJSON, err := buildBackupJob(ctx, &kb, &bkp, backupID)
		if err != nil {
			return ctrl.Result{}, err
		}
		cm := specConfigMap(bkp.Namespace, specConfigMapName(job.Name), specJSON)
		if cerr := r.Create(ctx, cm); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			if isQuotaExceeded(cerr) {
				bkp.Status.Phase = ragv1alpha1.BackupPhaseFailed
				setBackupCond(&bkp, metav1.ConditionFalse, "ResourceQuotaExceeded",
					"cannot create ConfigMap: resource quota exceeded")
				return ctrl.Result{}, r.Status().Update(ctx, &bkp)
			}
			return ctrl.Result{}, cerr
		}
		if cerr := r.Create(ctx, job); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			if isQuotaExceeded(cerr) {
				bkp.Status.Phase = ragv1alpha1.BackupPhaseFailed
				setBackupCond(&bkp, metav1.ConditionFalse, "ResourceQuotaExceeded",
					"cannot create Job: resource quota exceeded")
				return ctrl.Result{}, r.Status().Update(ctx, &bkp)
			}
			return ctrl.Result{}, cerr
		}
		bkp.Status.Phase = ragv1alpha1.BackupPhaseRunning
		bkp.Status.ActiveJob = job.Name
		setBackupCond(&bkp, metav1.ConditionFalse, "BackupJobCreated", "backup job started")
		return ctrl.Result{}, r.Status().Update(ctx, &bkp)
	}

	var job batchv1.Job
	jobKey := types.NamespacedName{Namespace: bkp.Namespace, Name: bkp.Status.ActiveJob}
	if err := r.Get(ctx, jobKey, &job); err != nil {
		if apierrors.IsNotFound(err) {
			bkp.Status.ActiveJob = ""
			return ctrl.Result{Requeue: true}, r.Status().Update(ctx, &bkp)
		}
		return ctrl.Result{}, err
	}

	if !jobComplete(&job) && !jobFailed(&job) {
		logger.Info("backup job still running", "job", job.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if jobFailed(&job) {
		bkp.Status.Phase = ragv1alpha1.BackupPhaseFailed
		setBackupCond(&bkp, metav1.ConditionFalse, "BackupFailed", "backup job failed")
		return ctrl.Result{}, r.Status().Update(ctx, &bkp)
	}

	var result BackupResult
	if err := readJobResult(ctx, r.Client, bkp.Namespace, job.Name, &result); err != nil {
		bkp.Status.Phase = ragv1alpha1.BackupPhaseFailed
		setBackupCond(&bkp, metav1.ConditionFalse, "ResultReadFailed", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, &bkp)
	}

	now := metav1.Now()
	bkp.Status.Phase = ragv1alpha1.BackupPhaseCompleted
	bkp.Status.CompletionTime = &now
	bkp.Status.TotalPoints = result.TotalPoints
	bkp.Status.SizeBytes = result.SizeBytes
	bkp.Status.Location = result.Location
	setBackupCond(&bkp, metav1.ConditionTrue, "BackupComplete", "backup completed successfully")
	deleteResultCM(ctx, r.Client, bkp.Namespace, job.Name)
	deleteSpecCM(ctx, r.Client, bkp.Namespace, job.Name)
	return ctrl.Result{}, r.Status().Update(ctx, &bkp)
}

func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ragv1alpha1.Backup{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

func setBackupCond(bkp *ragv1alpha1.Backup, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&bkp.Status.Conditions, metav1.Condition{
		Type:               ragv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}
