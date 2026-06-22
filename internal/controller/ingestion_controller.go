package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

// reconcileActiveJob finalizes status when the tracked Job finishes.
// Returns handled=true when it produced a terminal decision for this pass.
func (r *KnowledgeBaseReconciler) reconcileActiveJob(
	ctx context.Context, kb *ragv1alpha1.KnowledgeBase, eff ragv1alpha1.ChunkingSpec, hash, secretsHash string,
) (ctrl.Result, bool, error) {
	var job batchv1.Job
	key := types.NamespacedName{Namespace: kb.Namespace, Name: kb.Status.ActiveJob}
	if err := r.Get(ctx, key, &job); err != nil {
		if apierrors.IsNotFound(err) {
			// Job GC'd before we observed completion; clear and move on.
			kb.Status.ActiveJob = ""
			kb.Status.ActiveJobStartedAt = nil
			return ctrl.Result{}, false, r.statusUpdate(ctx, kb)
		}
		return ctrl.Result{}, true, err
	}

	if !jobComplete(&job) && !jobFailed(&job) {
		// Still running. If this is an ingest job and a cron-based freshness
		// ticks during the run, flag a deferred ingestion so it isn't silently dropped.
		if jobType(&job) == jobTypeIngest && kb.Spec.Freshness.Schedule != "" {
			var last time.Time
			if kb.Status.LastIndexedTime != nil {
				last = kb.Status.LastIndexedTime.Time
			}
			if cronDue(kb.Spec.Freshness.Schedule, last, time.Now()) {
				kb.Status.DeferCronIngest = true
				return ctrl.Result{RequeueAfter: 10 * time.Second}, true, r.statusUpdate(ctx, kb)
			}
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, true, nil
	}

	switch jobType(&job) {
	case jobTypeIngest:
		return r.finalizeIngest(ctx, kb, &job, jobEffectiveChunking(&job, eff), jobSpecHash(&job, hash), jobSecretsHash(&job, secretsHash))
	case jobTypeEval:
		return r.finalizeEval(ctx, kb, &job)
	default:
		kb.Status.ActiveJob = ""
		kb.Status.ActiveJobStartedAt = nil
		return ctrl.Result{}, true, r.statusUpdate(ctx, kb)
	}
}

func (r *KnowledgeBaseReconciler) finalizeIngest(
	ctx context.Context, kb *ragv1alpha1.KnowledgeBase, job *batchv1.Job, eff ragv1alpha1.ChunkingSpec, hash, secretsHash string,
) (ctrl.Result, bool, error) {
	if jobFailed(job) {
		// Read checkpoint before clearing access so the next attempt can resume.
		checkpoint := r.readCheckpointResult(ctx, kb.Namespace, job.Name)
		r.clearWorkerConfigMapAccess(ctx, kb)
		kb.Status.Phase = ragv1alpha1.PhaseFailed
		kb.Status.ActiveJob = ""
		kb.Status.ActiveJobStartedAt = nil
		kb.Status.LastCheckpoint = checkpoint
		now := metav1.Now()
		kb.Status.LastFailedSpecHash = hash
		kb.Status.LastFailureTime = &now
		ingestionsTotal.WithLabelValues(kb.Namespace, "failed").Inc()
		setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse, "IngestionFailed", "ingestion job failed")
		setCondition(kb, ragv1alpha1.ConditionIngesting, metav1.ConditionFalse, "Failed", "ingestion job failed")
		r.event(kb, corev1.EventTypeWarning, "IngestionFailed", "ingestion job %s failed", job.Name)
		if err := r.statusUpdate(ctx, kb); err != nil {
			return ctrl.Result{}, true, err
		}
		r.finalizeIngestionRun(ctx, kb.Namespace, job.Name, ragv1alpha1.IngestionRunFailed, 0, nil, "ingestion job failed")
		r.deleteResult(ctx, kb.Namespace, job.Name)
		r.deleteSpecConfigMap(ctx, kb.Namespace, job.Name)
		r.deleteCheckpointConfigMap(ctx, kb.Namespace, job.Name)
		r.deleteJob(ctx, job)
		return ctrl.Result{RequeueAfter: time.Minute}, true, nil
	}

	var result IngestResult
	if err := r.readResult(ctx, kb.Namespace, job.Name, &result); err != nil {
		// Result not yet visible; retry shortly.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, true, nil
	}
	r.clearWorkerConfigMapAccess(ctx, kb)

	kb.Status.Phase = ragv1alpha1.PhaseReady
	kb.Status.ObservedSpecHash = hash
	kb.Status.ObservedSecretsHash = secretsHash
	kb.Status.LastFailedSpecHash = ""
	kb.Status.LastFailureTime = nil
	kb.Status.LastCheckpoint = nil
	kb.Status.ObservedEmbeddingModel = kb.Spec.Embedding.Model
	emb := kb.Spec.Embedding.DeepCopy()
	kb.Status.ObservedEmbedding = emb
	kb.Status.EffectiveChunking = &eff
	kb.Status.PendingRetune = false // re-index satisfied
	kb.Status.IndexedChunks = result.TotalChunks
	kb.Status.Sources = toSourceStatus(result.Sources)
	now := metav1.Now()
	kb.Status.LastIndexedTime = &now
	kb.Status.ActiveJob = ""
	kb.Status.ActiveJobStartedAt = nil
	kb.Status.ObservedGeneration = kb.Generation

	indexedChunks.WithLabelValues(kb.Namespace).Set(float64(result.TotalChunks))
	observeSuccessfulIngestion(kb.Namespace, now.Time)
	ingestionsTotal.WithLabelValues(kb.Namespace, "succeeded").Inc()
	setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionTrue, "IngestionComplete",
		fmt.Sprintf("indexed %d chunks", result.TotalChunks))
	setCondition(kb, ragv1alpha1.ConditionIngesting, metav1.ConditionFalse, "Complete", "ingestion finished")
	r.event(kb, corev1.EventTypeNormal, "IngestionComplete", "indexed %d chunks", result.TotalChunks)
	if err := r.statusUpdate(ctx, kb); err != nil {
		return ctrl.Result{}, true, err
	}
	r.finalizeIngestionRun(ctx, kb.Namespace, job.Name, ragv1alpha1.IngestionRunSucceeded, result.TotalChunks, result.Sources, "")
	r.deleteResult(ctx, kb.Namespace, job.Name)
	r.deleteSpecConfigMap(ctx, kb.Namespace, job.Name)
	r.deleteCheckpointConfigMap(ctx, kb.Namespace, job.Name)
	r.deleteJob(ctx, job)
	return ctrl.Result{Requeue: true}, true, nil
}

func (r *KnowledgeBaseReconciler) finalizeIngestionRun(ctx context.Context, ns, name string, phase ragv1alpha1.IngestionRunPhase, chunks int, sources []ragv1alpha1.IngestSourceResult, errMsg string) {
	var ir ragv1alpha1.IngestionRun
	if e := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ir); e != nil {
		return // IngestionRun may not exist (legacy Jobs without IR)
	}
	now := metav1.Now()
	ir.Status.Phase = phase
	ir.Status.CompletionTime = &now
	ir.Status.TotalChunks = chunks
	ir.Status.Sources = toSourceStatus(sources)
	ir.Status.Error = errMsg
	if err := r.Status().Update(ctx, &ir); err != nil {
		log.FromContext(ctx).Error(err, "failed to finalize ingestion run", "ingestionRun", name)
	}
}

func (r *KnowledgeBaseReconciler) pruneIngestionRuns(ctx context.Context, ns, kbName string) {
	var list ragv1alpha1.IngestionRunList
	if err := r.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{labelKB: kbName}); err != nil {
		return
	}
	var terminal []ragv1alpha1.IngestionRun
	for i := range list.Items {
		if list.Items[i].Status.Phase == ragv1alpha1.IngestionRunSucceeded ||
			list.Items[i].Status.Phase == ragv1alpha1.IngestionRunFailed {
			terminal = append(terminal, list.Items[i])
		}
	}
	if len(terminal) <= 10 {
		return
	}
	// Sort by creation timestamp ascending so we delete oldest first.
	sort.Slice(terminal, func(i, j int) bool {
		return terminal[i].CreationTimestamp.Before(&terminal[j].CreationTimestamp)
	})
	for i := 0; i < len(terminal)-10; i++ {
		_ = r.Delete(ctx, &terminal[i])
	}
}

func (r *KnowledgeBaseReconciler) startIngest(
	ctx context.Context, kb *ragv1alpha1.KnowledgeBase, eff ragv1alpha1.ChunkingSpec, hash, secretsHash, reason string,
) (ctrl.Result, error) {
	// A deferred cron ingest is now being honoured — clear the flag.
	kb.Status.DeferCronIngest = false

	// Incremental sync (per-source revision skip) is only safe when the *reason*
	// is a freshness re-sync: the spec is unchanged, so a source skips iff its
	// upstream revision is unchanged. Any spec change (sources, globs, chunking,
	// model) or an initial run must fully re-process, because the revision marker
	// alone cannot detect those.
	mode := ragv1alpha1.IngestFull
	if strings.HasPrefix(reason, "freshness") || strings.HasPrefix(reason, "deferred") {
		mode = kb.Spec.Ingestion.Mode
		if mode == "" {
			mode = ragv1alpha1.IngestIncremental
		}
	}

	kb.Status.IngestRound++
	job, specJSON, err := buildIngestJob(ctx, kb, hash, secretsHash, mode, eff)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !specConfigMapSizeOK(specJSON) {
		kb.Status.Phase = ragv1alpha1.PhaseFailed
		setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse,
			"SpecConfigTooLarge", "worker spec ConfigMap exceeds 1 MiB limit; reduce includeGlobs or web URLs, or split into multiple KnowledgeBases")
		return ctrl.Result{}, r.statusUpdate(ctx, kb)
	}
	// Create spec ConfigMap first so the Job volume mount resolves.
	cm := specConfigMap(kb.Namespace, specConfigMapName(job.Name), specJSON)
	if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		if isQuotaExceeded(err) {
			setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse,
				"ResourceQuotaExceeded", "cannot create ConfigMap: resource quota exceeded")
			if updateErr := r.statusUpdate(ctx, kb); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			r.event(kb, corev1.EventTypeWarning, "ResourceQuotaExceeded",
				"resource quota prevents creating spec ConfigMap")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		return ctrl.Result{}, err
	}
	if err := r.prepareWorkerJob(ctx, kb, job.Name); err != nil {
		if isQuotaExceeded(err) {
			setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse,
				"ResourceQuotaExceeded", "cannot create worker resources: resource quota exceeded")
			if updateErr := r.statusUpdate(ctx, kb); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			r.event(kb, corev1.EventTypeWarning, "ResourceQuotaExceeded",
				"resource quota prevents creating worker identity/resources")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		return ctrl.Result{}, err
	}
	if err := ctrl.SetControllerReference(kb, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil {
		if isQuotaExceeded(err) {
			setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse,
				"ResourceQuotaExceeded", "cannot create Job: resource quota exceeded")
			if updateErr := r.statusUpdate(ctx, kb); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			r.event(kb, corev1.EventTypeWarning, "ResourceQuotaExceeded",
				"resource quota prevents creating worker Job")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		return ctrl.Result{}, ignoreAlreadyExists(err)
	}

	// Create an immutable IngestionRun for auditing.
	ir := &ragv1alpha1.IngestionRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: kb.Namespace,
			Labels:    map[string]string{labelKB: kb.Name, labelManagedBy: "kuberag"},
		},
		Spec: ragv1alpha1.IngestionRunSpec{
			KnowledgeBaseRef:  ragv1alpha1.LocalObjectRef{Name: kb.Name},
			Mode:              mode,
			SpecHash:          hash,
			EffectiveChunking: eff,
		},
	}
	_ = ctrl.SetControllerReference(kb, ir, r.Scheme)
	if err := r.Create(ctx, ir); err != nil {
		if isQuotaExceeded(err) {
			setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse,
				"ResourceQuotaExceeded", "cannot create IngestionRun: resource quota exceeded")
			if updateErr := r.statusUpdate(ctx, kb); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			r.event(kb, corev1.EventTypeWarning, "ResourceQuotaExceeded",
				"resource quota prevents creating IngestionRun")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ir.Namespace, Name: ir.Name}, ir); err != nil {
			return ctrl.Result{}, err
		}
	}
	if ir.Status.Phase == "" {
		ir.Status.Phase = ragv1alpha1.IngestionRunRunning
		ir.Status.StartTime = &metav1.Time{Time: time.Now()}
		if err := r.Status().Update(ctx, ir); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.pruneIngestionRuns(ctx, kb.Namespace, kb.Name)

	kb.Status.Phase = ragv1alpha1.PhaseIngesting
	kb.Status.ActiveJob = job.Name
	now := metav1.Now()
	kb.Status.ActiveJobStartedAt = &now
	setCondition(kb, ragv1alpha1.ConditionIngesting, metav1.ConditionTrue, "JobCreated",
		fmt.Sprintf("%s ingestion started (%s)", mode, reason))
	setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse, "Ingesting", "ingestion in progress")
	r.event(kb, corev1.EventTypeNormal, "IngestionStarted", "%s ingestion: %s", mode, reason)
	if err := r.statusUpdate(ctx, kb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}
