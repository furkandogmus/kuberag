package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

const (
	finalizer = "rag.furkan.dev/finalizer"
	ownerKey  = ".metadata.controller"
)

// KnowledgeBaseReconciler reconciles a KnowledgeBase object.
type KnowledgeBaseReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=rag.furkan.dev,resources=knowledgebases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=knowledgebases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=knowledgebases/finalizers,verbs=update
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=vectorindices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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

	eff := effectiveChunking(&kb)
	hash := specHash(&kb, eff)

	// Finalize any in-flight job before deciding new work.
	if kb.Status.ActiveJob != "" {
		res, handled, err := r.reconcileActiveJob(ctx, &kb, eff, hash)
		if err != nil || handled {
			return res, err
		}
	}

	// 1) Ingestion takes priority over evaluation.
	if reason, need := needsIngest(&kb, hash); need {
		return r.startIngest(ctx, &kb, eff, hash, reason)
	}

	// 2) Retrieval-quality evaluation.
	if evalDue(&kb) {
		return r.startEval(ctx, &kb, eff, hash)
	}

	// 3) Steady state: requeue near the next scheduled freshness/eval fire.
	logger.V(1).Info("steady state", "knowledgebase", kb.Name, "phase", kb.Status.Phase)
	now := time.Now()
	next := requeueFor(nextFire(kb.Spec.Freshness.Schedule, now), nextEvalFire(&kb, now))
	return ctrl.Result{RequeueAfter: next}, nil
}

// reconcileActiveJob finalizes status when the tracked Job finishes.
// Returns handled=true when it produced a terminal decision for this pass.
func (r *KnowledgeBaseReconciler) reconcileActiveJob(
	ctx context.Context, kb *ragv1alpha1.KnowledgeBase, eff ragv1alpha1.ChunkingSpec, hash string,
) (ctrl.Result, bool, error) {
	var job batchv1.Job
	key := types.NamespacedName{Namespace: kb.Namespace, Name: kb.Status.ActiveJob}
	if err := r.Get(ctx, key, &job); err != nil {
		if apierrors.IsNotFound(err) {
			// Job GC'd before we observed completion; clear and move on.
			kb.Status.ActiveJob = ""
			return ctrl.Result{}, false, r.statusUpdate(ctx, kb)
		}
		return ctrl.Result{}, true, err
	}

	if !jobComplete(&job) && !jobFailed(&job) {
		// Still running.
		return ctrl.Result{RequeueAfter: 10 * time.Second}, true, nil
	}

	switch jobType(&job) {
	case jobTypeIngest:
		return r.finalizeIngest(ctx, kb, &job, eff, hash)
	case jobTypeEval:
		return r.finalizeEval(ctx, kb, &job)
	default:
		kb.Status.ActiveJob = ""
		return ctrl.Result{}, true, r.statusUpdate(ctx, kb)
	}
}

func (r *KnowledgeBaseReconciler) finalizeIngest(
	ctx context.Context, kb *ragv1alpha1.KnowledgeBase, job *batchv1.Job, eff ragv1alpha1.ChunkingSpec, hash string,
) (ctrl.Result, bool, error) {
	defer r.deleteResult(ctx, kb.Namespace, job.Name)

	if jobFailed(job) {
		kb.Status.Phase = ragv1alpha1.PhaseFailed
		kb.Status.ActiveJob = ""
		ingestionsTotal.WithLabelValues(kb.Name, "failed").Inc()
		setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse, "IngestionFailed", "ingestion job failed")
		setCondition(kb, ragv1alpha1.ConditionIngesting, metav1.ConditionFalse, "Failed", "ingestion job failed")
		r.event(kb, corev1.EventTypeWarning, "IngestionFailed", "ingestion job %s failed", job.Name)
		if err := r.statusUpdate(ctx, kb); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, true, nil
	}

	var result IngestResult
	if err := r.readResult(ctx, kb.Namespace, job.Name, &result); err != nil {
		// Result not yet visible; retry shortly.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, true, nil
	}

	kb.Status.Phase = ragv1alpha1.PhaseReady
	kb.Status.ObservedSpecHash = hash
	kb.Status.ObservedEmbeddingModel = kb.Spec.Embedding.Model
	kb.Status.EffectiveChunking = &eff
	kb.Status.IndexedChunks = result.TotalChunks
	kb.Status.Sources = toSourceStatus(result.Sources)
	now := metav1.Now()
	kb.Status.LastIndexedTime = &now
	kb.Status.ActiveJob = ""
	kb.Status.ObservedGeneration = kb.Generation

	indexedChunks.WithLabelValues(kb.Name).Set(float64(result.TotalChunks))
	ingestionsTotal.WithLabelValues(kb.Name, "succeeded").Inc()
	setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionTrue, "IngestionComplete",
		fmt.Sprintf("indexed %d chunks", result.TotalChunks))
	setCondition(kb, ragv1alpha1.ConditionIngesting, metav1.ConditionFalse, "Complete", "ingestion finished")
	r.event(kb, corev1.EventTypeNormal, "IngestionComplete", "indexed %d chunks", result.TotalChunks)
	if err := r.statusUpdate(ctx, kb); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{Requeue: true}, true, nil
}

func (r *KnowledgeBaseReconciler) finalizeEval(
	ctx context.Context, kb *ragv1alpha1.KnowledgeBase, job *batchv1.Job,
) (ctrl.Result, bool, error) {
	defer r.deleteResult(ctx, kb.Namespace, job.Name)
	kb.Status.ActiveJob = ""

	if jobFailed(job) {
		setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionFalse, "EvalFailed", "evaluation job failed")
		r.event(kb, corev1.EventTypeWarning, "EvalFailed", "evaluation job %s failed", job.Name)
		return ctrl.Result{Requeue: true}, true, r.statusUpdate(ctx, kb)
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

	rq := kb.Spec.RetrievalQuality
	target := rq.MinimumRecallPercent
	below := target > 0 && result.RecallPercent < target

	if !below {
		kb.Status.Phase = ragv1alpha1.PhaseReady
		setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionTrue, "RecallMet",
			fmt.Sprintf("recall %d%% >= target %d%%", result.RecallPercent, target))
		r.event(kb, corev1.EventTypeNormal, "RecallMet", "recall %d%% meets target %d%%", result.RecallPercent, target)
		return ctrl.Result{Requeue: true}, true, r.statusUpdate(ctx, kb)
	}

	// Below target.
	if autoTuneEnabled(rq) && kb.Status.AutoTuneAttempts < autoTuneMax(rq) {
		applyAutoTune(kb)
		autoTuneAttempts.WithLabelValues(kb.Name).Set(float64(kb.Status.AutoTuneAttempts))
		setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionFalse, "AutoTuning",
			fmt.Sprintf("recall %d%% < target %d%%; tuning chunking (attempt %d)",
				result.RecallPercent, target, kb.Status.AutoTuneAttempts))
		r.event(kb, corev1.EventTypeNormal, "AutoTuning",
			"recall %d%% below target %d%%, re-indexing with tuned chunking (attempt %d/%d)",
			result.RecallPercent, target, kb.Status.AutoTuneAttempts, autoTuneMax(rq))
		// Re-index will be triggered next pass because ObservedSpecHash was cleared.
		return ctrl.Result{Requeue: true}, true, r.statusUpdate(ctx, kb)
	}

	kb.Status.Phase = ragv1alpha1.PhaseDegraded
	setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse, "RecallBelowTarget",
		fmt.Sprintf("recall %d%% < target %d%%", result.RecallPercent, target))
	setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionFalse, "RecallBelowTarget",
		fmt.Sprintf("recall %d%% < target %d%%", result.RecallPercent, target))
	r.event(kb, corev1.EventTypeWarning, "RecallBelowTarget",
		"recall %d%% below target %d%% and auto-tune exhausted", result.RecallPercent, target)
	return ctrl.Result{Requeue: true}, true, r.statusUpdate(ctx, kb)
}

func (r *KnowledgeBaseReconciler) startIngest(
	ctx context.Context, kb *ragv1alpha1.KnowledgeBase, eff ragv1alpha1.ChunkingSpec, hash, reason string,
) (ctrl.Result, error) {
	// Incremental sync (per-source revision skip) is only safe when the *reason*
	// is a freshness re-sync: the spec is unchanged, so a source skips iff its
	// upstream revision is unchanged. Any spec change (sources, globs, chunking,
	// model) or an initial run must fully re-process, because the revision marker
	// alone cannot detect those.
	mode := ragv1alpha1.IngestFull
	if strings.HasPrefix(reason, "freshness") {
		mode = kb.Spec.Ingestion.Mode
		if mode == "" {
			mode = ragv1alpha1.IngestIncremental
		}
	}

	job, err := buildIngestJob(kb, hash, mode, eff)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := ctrl.SetControllerReference(kb, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil {
		return ctrl.Result{}, ignoreAlreadyExists(err)
	}

	kb.Status.Phase = ragv1alpha1.PhaseIngesting
	kb.Status.ActiveJob = job.Name
	setCondition(kb, ragv1alpha1.ConditionIngesting, metav1.ConditionTrue, "JobCreated",
		fmt.Sprintf("%s ingestion started (%s)", mode, reason))
	setCondition(kb, ragv1alpha1.ConditionReady, metav1.ConditionFalse, "Ingesting", "ingestion in progress")
	r.event(kb, corev1.EventTypeNormal, "IngestionStarted", "%s ingestion: %s", mode, reason)
	if err := r.statusUpdate(ctx, kb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *KnowledgeBaseReconciler) startEval(
	ctx context.Context, kb *ragv1alpha1.KnowledgeBase, eff ragv1alpha1.ChunkingSpec, hash string,
) (ctrl.Result, error) {
	kb.Status.EvalRound++
	job, err := buildEvalJob(kb, hash, kb.Status.EvalRound, eff)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := ctrl.SetControllerReference(kb, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil {
		return ctrl.Result{}, ignoreAlreadyExists(err)
	}
	kb.Status.ActiveJob = job.Name
	setCondition(kb, ragv1alpha1.ConditionEvaluated, metav1.ConditionFalse, "Evaluating", "running retrieval-quality evaluation")
	r.event(kb, corev1.EventTypeNormal, "EvaluationStarted", "running retrieval-quality evaluation")
	if err := r.statusUpdate(ctx, kb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

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
		cj, berr := buildCleanupJob(kb)
		if berr != nil {
			return ctrl.Result{}, berr
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
	// Cleanup finished (success or give-up). Remove the cleanup Job and finalizer.
	_ = r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	controllerutil.RemoveFinalizer(kb, finalizer)
	if err := r.Update(ctx, kb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// ensureVectorIndex creates/updates the VectorIndex tracking this KB's collection.
func (r *KnowledgeBaseReconciler) ensureVectorIndex(ctx context.Context, kb *ragv1alpha1.KnowledgeBase) error {
	name := truncName(kb.Name + "-index")
	desired := &ragv1alpha1.VectorIndex{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: kb.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, desired, func() error {
		desired.Spec.KnowledgeBaseRef = ragv1alpha1.LocalObjectRef{Name: kb.Name}
		desired.Spec.Store = kb.Spec.VectorStore
		if desired.Spec.Store.Collection == "" {
			desired.Spec.Store.Collection = kb.Name
		}
		desired.Spec.Dimension = embeddingDimension(kb.Spec.Embedding)
		if desired.Spec.ProbeIntervalSeconds == 0 {
			desired.Spec.ProbeIntervalSeconds = 60
		}
		return controllerutil.SetControllerReference(kb, desired, r.Scheme)
	})
	return err
}

func (r *KnowledgeBaseReconciler) statusUpdate(ctx context.Context, kb *ragv1alpha1.KnowledgeBase) error {
	return r.Status().Update(ctx, kb)
}

func (r *KnowledgeBaseReconciler) event(obj runtime.Object, etype, reason, msg string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(obj, etype, reason, msg, args...)
}

// ---------------------------------------------------------------------------
// Pure decision helpers (unit-tested)
// ---------------------------------------------------------------------------

// effectiveChunking returns the chunking actually in use: the auto-tuned values
// from status if present, otherwise the spec with defaults filled.
func effectiveChunking(kb *ragv1alpha1.KnowledgeBase) ragv1alpha1.ChunkingSpec {
	if kb.Status.EffectiveChunking != nil {
		return *kb.Status.EffectiveChunking
	}
	c := kb.Spec.Chunking
	if c.Strategy == "" {
		c.Strategy = ragv1alpha1.ChunkSemantic
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = 800
	}
	if c.Overlap == 0 {
		c.Overlap = 80
	}
	return c
}

// specHash fingerprints the spec fields that require re-ingestion when changed.
func specHash(kb *ragv1alpha1.KnowledgeBase, eff ragv1alpha1.ChunkingSpec) string {
	material := struct {
		Sources  []ragv1alpha1.Source
		Chunking ragv1alpha1.ChunkingSpec
		Model    string
		Store    string
	}{
		Sources:  kb.Spec.Sources,
		Chunking: eff,
		Model:    kb.Spec.Embedding.Model,
		Store:    string(kb.Spec.VectorStore.Type) + "|" + kb.Spec.VectorStore.Endpoint,
	}
	b, _ := json.Marshal(material)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// needsIngest decides whether the store is stale relative to the spec.
func needsIngest(kb *ragv1alpha1.KnowledgeBase, desiredHash string) (reason string, need bool) {
	if kb.Status.ObservedSpecHash == "" {
		return "initial ingestion", true
	}
	if kb.Status.ObservedEmbeddingModel != kb.Spec.Embedding.Model {
		return fmt.Sprintf("embedding model changed %q -> %q",
			kb.Status.ObservedEmbeddingModel, kb.Spec.Embedding.Model), true
	}
	if kb.Status.ObservedSpecHash != desiredHash {
		return "spec changed (sources/chunking/store)", true
	}
	if kb.Spec.Freshness.Schedule != "" {
		var last time.Time
		if kb.Status.LastIndexedTime != nil {
			last = kb.Status.LastIndexedTime.Time
		}
		if cronDue(kb.Spec.Freshness.Schedule, last, time.Now()) {
			return "freshness window elapsed", true
		}
	}
	return "", false
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

// applyAutoTune adjusts effective chunking to chase a higher recall and forces
// a re-index by clearing the observed spec hash.
func applyAutoTune(kb *ragv1alpha1.KnowledgeBase) {
	eff := effectiveChunking(kb)
	// Strategy: grow overlap first; once overlap is large relative to chunk size,
	// shrink the chunk to increase granularity.
	eff.Overlap += 40
	if eff.Overlap > eff.MaxTokens/2 {
		if eff.MaxTokens > 300 {
			eff.MaxTokens -= 200
		}
		eff.Overlap = eff.MaxTokens / 5
	}
	kb.Status.EffectiveChunking = &eff
	kb.Status.AutoTuneAttempts++
	kb.Status.ObservedSpecHash = "" // forces re-ingest on next pass
	// Clear the last evaluation so a re-eval runs after the tuned re-index even
	// when no evalSchedule is set; this lets auto-tune iterate up to maxAttempts.
	kb.Status.Evaluation = nil
}

func autoTuneEnabled(rq *ragv1alpha1.RetrievalQualitySpec) bool {
	return rq != nil && rq.AutoTune != nil && rq.AutoTune.Enabled != nil && *rq.AutoTune.Enabled
}

func autoTuneMax(rq *ragv1alpha1.RetrievalQualitySpec) int {
	if rq == nil || rq.AutoTune == nil || rq.AutoTune.MaxAttempts == 0 {
		return 3
	}
	return rq.AutoTune.MaxAttempts
}

// embeddingDimension resolves the expected vector dimension for an embedding
// spec: explicit override first, then a built-in table of known models, then a
// sensible local default. Returns 0 for unknown hosted models, signalling the
// worker to auto-detect and the VectorIndex probe to skip the dimension check.
func embeddingDimension(e ragv1alpha1.EmbeddingSpec) int {
	if e.Dimension > 0 {
		return e.Dimension
	}
	switch e.Model {
	case "bge-small":
		return 384
	case "bge-large":
		return 1024
	case "text-embedding-004", "text-embedding-005":
		return 768
	case "text-embedding-3-small":
		return 1536
	case "text-embedding-3-large", "gemini-embedding-001":
		return 3072
	}
	if e.Provider == "" || e.Provider == "local" {
		return 384 // fastembed default
	}
	return 0 // hosted, unknown -> auto-detected at ingest time
}

func toSourceStatus(in []IngestSourceResult) []ragv1alpha1.SourceStatus {
	out := make([]ragv1alpha1.SourceStatus, 0, len(in))
	for _, s := range in {
		out = append(out, ragv1alpha1.SourceStatus{Name: s.Name, Revision: s.Revision, Chunks: s.Chunks})
	}
	return out
}

func jobComplete(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailed(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func setCondition(kb *ragv1alpha1.KnowledgeBase, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&kb.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: kb.Generation,
	})
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
		Complete(r)
}
