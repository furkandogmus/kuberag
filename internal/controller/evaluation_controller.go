package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

func (r *KnowledgeBaseReconciler) startEval(
	ctx context.Context, kb *ragv1alpha1.KnowledgeBase, eff ragv1alpha1.ChunkingSpec, hash, secretsHash string,
) (ctrl.Result, error) {
	kb.Status.EvalRound++
	job, specJSON, err := buildEvalJob(kb, hash, secretsHash, kb.Status.EvalRound, eff)
	if err != nil {
		return ctrl.Result{}, err
	}
	cm := specConfigMap(kb.Namespace, specConfigMapName(job.Name), specJSON)
	if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	if err := ctrl.SetControllerReference(kb, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil {
		return ctrl.Result{}, ignoreAlreadyExists(err)
	}
	kb.Status.ActiveJob = job.Name
	now := metav1.Now()
	kb.Status.ActiveJobStartedAt = &now
	setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionFalse, "Evaluating", "running retrieval-quality evaluation")
	r.event(kb, corev1.EventTypeNormal, "EvaluationStarted", "running retrieval-quality evaluation")
	if err := r.statusUpdate(ctx, kb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *KnowledgeBaseReconciler) finalizeEval(
	ctx context.Context, kb *ragv1alpha1.KnowledgeBase, job *batchv1.Job,
) (ctrl.Result, bool, error) {
	kb.Status.ActiveJob = ""
	kb.Status.ActiveJobStartedAt = nil

	if jobFailed(job) {
		setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionFalse, "EvalFailed", "evaluation job failed")
		r.event(kb, corev1.EventTypeWarning, "EvalFailed", "evaluation job %s failed", job.Name)
		if err := r.statusUpdate(ctx, kb); err != nil {
			return ctrl.Result{}, true, err
		}
		r.deleteResult(ctx, kb.Namespace, job.Name)
		r.deleteSpecConfigMap(ctx, kb.Namespace, job.Name)
		return ctrl.Result{Requeue: true}, true, nil
	}

	var result EvalResult
	if err := r.readResult(ctx, kb.Namespace, job.Name, &result); err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, true, nil
	}

	now := metav1.Now()
	kb.Status.Evaluation = &ragv1alpha1.EvaluationStatus{
		RecallPercent:    result.RecallPercent,
		P95LatencyMillis: result.P95LatencyMillis,
		Queries:          result.Queries,
		Time:             &now,
	}
	retrievalRecall.WithLabelValues(kb.Name).Set(float64(result.RecallPercent))

	// No queries means the dataset is empty/missing: recall 0% is meaningless, so
	// don't gate, auto-tune, or degrade on it. Evaluation is still recorded (with a
	// timestamp) so this doesn't busy-loop re-evaluating.
	if result.Queries == 0 {
		setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionFalse, "NoDataset",
			"evaluation dataset empty; recall gate skipped — add queries to the dataset ConfigMap")
		r.event(kb, corev1.EventTypeWarning, "EvalNoDataset",
			"evaluation dataset empty; skipping recall gate and auto-tune")
		if err := r.statusUpdate(ctx, kb); err != nil {
			return ctrl.Result{}, true, err
		}
		r.deleteResult(ctx, kb.Namespace, job.Name)
		r.deleteSpecConfigMap(ctx, kb.Namespace, job.Name)
		return ctrl.Result{Requeue: true}, true, nil
	}

	rq := kb.Spec.RetrievalQuality
	target := rq.MinimumRecallPercent
	below := target > 0 && result.RecallPercent < target

	if !below {
		kb.Status.Phase = ragv1alpha1.PhaseReady
		recordAutoTuneDuration(kb, "converged", time.Now())
		kb.Status.AutoTuneAttempts = 0
		kb.Status.BestChunking = nil
		kb.Status.BestRecallPercent = 0
		autoTuneAttempts.WithLabelValues(kb.Name).Set(0)
		autoTuneBestRecall.WithLabelValues(kb.Name).Set(float64(result.RecallPercent))
		setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionTrue, "RecallMet",
			fmt.Sprintf("recall %d%% >= target %d%%", result.RecallPercent, target))
		r.event(kb, corev1.EventTypeNormal, "RecallMet", "recall %d%% meets target %d%%", result.RecallPercent, target)
		if err := r.statusUpdate(ctx, kb); err != nil {
			return ctrl.Result{}, true, err
		}
		r.deleteResult(ctx, kb.Namespace, job.Name)
		r.deleteSpecConfigMap(ctx, kb.Namespace, job.Name)
		return ctrl.Result{Requeue: true}, true, nil
	}

	// Below target. Remember the best config we've seen before stepping further,
	// so auto-tune can land on it later even if subsequent steps regress.
	recordBest(kb, result.RecallPercent)
	autoTuneBestRecall.WithLabelValues(kb.Name).Set(float64(kb.Status.BestRecallPercent))

	if autoTuneEnabled(rq) {
		if kb.Status.AutoTuneAttempts < autoTuneMax(rq) {
			applyAutoTune(kb)
			autoTuneAttempts.WithLabelValues(kb.Name).Set(float64(kb.Status.AutoTuneAttempts))
			setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionFalse, "AutoTuning",
				fmt.Sprintf("recall %d%% < target %d%%; tuning chunking (attempt %d)",
					result.RecallPercent, target, kb.Status.AutoTuneAttempts))
			r.event(kb, corev1.EventTypeNormal, "AutoTuning",
				"recall %d%% below target %d%%, re-indexing with tuned chunking (attempt %d/%d)",
				result.RecallPercent, target, kb.Status.AutoTuneAttempts, autoTuneMax(rq))
			if err := r.statusUpdate(ctx, kb); err != nil {
				return ctrl.Result{}, true, err
			}
			r.deleteResult(ctx, kb.Namespace, job.Name)
			r.deleteSpecConfigMap(ctx, kb.Namespace, job.Name)
			return ctrl.Result{Requeue: true}, true, nil
		}

		// Attempts exhausted. Land on the best config observed rather than the
		// last (arbitrary) ladder step. settleOnBest forces one final re-index;
		// the subsequent re-eval finds current == best and falls through to Degraded.
		recordAutoTuneDuration(kb, "exhausted", time.Now())
		if settleOnBest(kb) {
			setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionFalse, "AutoTuneSettling",
				fmt.Sprintf("auto-tune exhausted; reverting to best config (recall %d%%)",
					kb.Status.BestRecallPercent))
			r.event(kb, corev1.EventTypeNormal, "AutoTuneSettling",
				"auto-tune exhausted, re-indexing with best config seen (recall %d%% < target %d%%)",
				kb.Status.BestRecallPercent, target)
			if err := r.statusUpdate(ctx, kb); err != nil {
				return ctrl.Result{}, true, err
			}
			r.deleteResult(ctx, kb.Namespace, job.Name)
			r.deleteSpecConfigMap(ctx, kb.Namespace, job.Name)
			return ctrl.Result{Requeue: true}, true, nil
		}
	}

	kb.Status.Phase = ragv1alpha1.PhaseDegraded
	setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse, "RecallBelowTarget",
		fmt.Sprintf("recall %d%% < target %d%% (best %d%%)", result.RecallPercent, target, kb.Status.BestRecallPercent))
	setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionFalse, "RecallBelowTarget",
		fmt.Sprintf("recall %d%% < target %d%% (best %d%%)", result.RecallPercent, target, kb.Status.BestRecallPercent))
	r.event(kb, corev1.EventTypeWarning, "RecallBelowTarget",
		"recall %d%% below target %d%% and auto-tune exhausted (best %d%%)",
		result.RecallPercent, target, kb.Status.BestRecallPercent)
	if err := r.statusUpdate(ctx, kb); err != nil {
		return ctrl.Result{}, true, err
	}
	r.deleteResult(ctx, kb.Namespace, job.Name)
	r.deleteSpecConfigMap(ctx, kb.Namespace, job.Name)
	return ctrl.Result{Requeue: true}, true, nil
}

// evalDue reports whether a retrieval-quality evaluation should run now.
func evalDue(kb *ragv1alpha1.KnowledgeBase) bool {
	rq := kb.Spec.RetrievalQuality
	if rq == nil || (rq.Enabled != nil && !*rq.Enabled) {
		return false
	}
	// Only evaluate once data exists.
	if kb.Status.Phase != ragv1alpha1.PhaseReady && kb.Status.Phase != ragv1alpha1.PhaseDegraded {
		return false
	}
	if kb.Status.IndexedChunks == 0 {
		return false
	}
	if kb.Status.Evaluation == nil {
		return true
	}
	if rq.EvalSchedule == "" {
		return false
	}
	var last time.Time
	if kb.Status.Evaluation.Time != nil {
		last = kb.Status.Evaluation.Time.Time
	}
	return cronDue(rq.EvalSchedule, last, time.Now())
}

func nextEvalFire(kb *ragv1alpha1.KnowledgeBase, now time.Time) time.Time {
	if kb.Spec.RetrievalQuality == nil {
		return time.Time{}
	}
	return nextFire(kb.Spec.RetrievalQuality.EvalSchedule, now)
}
