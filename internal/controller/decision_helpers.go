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

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Pure decision helpers (unit-tested)
// ---------------------------------------------------------------------------

// effectiveChunking returns the chunking actually in use: the auto-tuned values
// from status if present, otherwise the spec with defaults filled.
// specChunking is the user's requested chunking with defaults filled in.
func specChunking(kb *ragv1alpha1.KnowledgeBase) ragv1alpha1.ChunkingSpec {
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

// effectiveChunking is the chunking actually used: an auto-tuned override from
// status if present, otherwise the user's spec.
func effectiveChunking(kb *ragv1alpha1.KnowledgeBase) ragv1alpha1.ChunkingSpec {
	if kb.Status.EffectiveChunking != nil {
		return *kb.Status.EffectiveChunking
	}
	return specChunking(kb)
}

// corpusHash fingerprints the knowledge content spec (sources, chunking, embedding,
// vector store) so changes that alter the indexed corpus trigger re-ingestion.
// Secret values are deliberately excluded — credential rotation should not force a
// full re-index. Secrets are tracked separately via ObservedSecretsHash and only
// cause in-flight Job cancellation, not re-ingestion.
func corpusHash(kb *ragv1alpha1.KnowledgeBase) string {
	material := struct {
		Sources     []ragv1alpha1.Source
		Chunking    ragv1alpha1.ChunkingSpec
		Embedding   ragv1alpha1.EmbeddingSpec
		VectorStore ragv1alpha1.VectorStoreSpec
	}{
		Sources:     kb.Spec.Sources,
		Chunking:    specChunking(kb),
		Embedding:   kb.Spec.Embedding,
		VectorStore: kb.Spec.VectorStore,
	}
	b, _ := json.Marshal(material)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// chunkFingerprint is a short, stable fingerprint of an effective chunking
// config. It disambiguates ingest Job names: a settle/revert re-index reuses the
// same AutoTuneAttempts counter as the preceding attempt but re-indexes a
// *different* chunking, so without folding the chunking in, the two Jobs would
// collide on name. The colliding completed Job's result ConfigMap is already
// gone, which would stall the loop.
func chunkFingerprint(c ragv1alpha1.ChunkingSpec) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", c.Strategy, c.MaxTokens, c.Overlap)))
	return hex.EncodeToString(sum[:3])
}

// userEditedSpec reports whether the user changed the spec (hash drift) while an
// auto-tuned chunking override is active, so the override must be discarded. It
// holds mid-tune too: ObservedSpecHash always tracks the last ingested spec (the
// forced re-index uses PendingRetune, not a cleared hash), so an edit during the
// re-index window is still detected instead of being masked by the override.
func userEditedSpec(kb *ragv1alpha1.KnowledgeBase, desiredHash string) bool {
	return kb.Status.EffectiveChunking != nil &&
		kb.Status.ObservedSpecHash != "" &&
		kb.Status.ObservedSpecHash != desiredHash
}

// needsIngest decides whether the store is stale relative to the spec.
func needsIngest(kb *ragv1alpha1.KnowledgeBase, desiredHash string) (reason string, need bool) {
	if kb.Status.DeferCronIngest && kb.Status.ActiveJob == "" {
		return "deferred cron ingestion", true
	}
	if kb.Status.PendingRetune {
		return "auto-tune re-index", true
	}
	if kb.Status.ObservedSpecHash == "" {
		return "initial ingestion", true
	}
	if kb.Status.ObservedEmbeddingModel != kb.Spec.Embedding.Model {
		return fmt.Sprintf("embedding model changed %q -> %q",
			kb.Status.ObservedEmbeddingModel, kb.Spec.Embedding.Model), true
	}
	if kb.Status.ObservedSpecHash != desiredHash {
		return "spec changed (sources/chunking/embedding/store)", true
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

// chunkFloor is the smallest maxTokens auto-tune will shrink a chunk to.
const chunkFloor = 300

// nextChunking is the pure auto-tune ladder: it returns the next chunking to try.
//
//  1. Grow overlap (+40) to stop answers being cut across chunk boundaries.
//  2. Once overlap dominates the chunk, shrink maxTokens (-200, floor 300) and
//     reset overlap to maxTokens/5 — finer-grained, more precise chunks.
//  3. Once chunks are at the floor and overlap already dominates, rotate the
//     split strategy and reset size/overlap — attack the corpus with a different
//     boundary model instead of shrinking further.
func nextChunking(c ragv1alpha1.ChunkingSpec) ragv1alpha1.ChunkingSpec {
	// At the chunk-size floor with overlap that can no longer grow without
	// dominating: the size ladder is exhausted, so rotate the split strategy and
	// reset size/overlap rather than churning the same small chunks.
	if c.MaxTokens <= chunkFloor && c.Overlap+40 > c.MaxTokens/2 {
		c.Strategy = rotateStrategy(c.Strategy)
		c.MaxTokens = 800
		c.Overlap = 80
		return c
	}
	c.Overlap += 40
	if c.Overlap > c.MaxTokens/2 {
		if c.MaxTokens > chunkFloor {
			c.MaxTokens -= 200
			if c.MaxTokens < chunkFloor {
				c.MaxTokens = chunkFloor
			}
		}
		c.Overlap = c.MaxTokens / 5
	}
	return c
}

// rotateStrategy cycles through the available split strategies so auto-tune can
// explore boundary models, not just chunk sizes.
func rotateStrategy(s ragv1alpha1.ChunkingStrategy) ragv1alpha1.ChunkingStrategy {
	switch s {
	case ragv1alpha1.ChunkSemantic:
		return ragv1alpha1.ChunkRecursive
	case ragv1alpha1.ChunkRecursive:
		return ragv1alpha1.ChunkFixed
	default:
		return ragv1alpha1.ChunkSemantic
	}
}

// applyAutoTune steps the effective chunking along the ladder and forces a
// re-index by marking PendingRetune.
func applyAutoTune(kb *ragv1alpha1.KnowledgeBase) {
	eff := nextChunking(effectiveChunking(kb))
	kb.Status.EffectiveChunking = &eff
	kb.Status.AutoTuneAttempts++
	if kb.Status.AutoTuneStartedAt == nil {
		now := metav1.Now()
		kb.Status.AutoTuneStartedAt = &now
	}
	kb.Status.PendingRetune = true // owes a re-ingest on the next pass
	// Clear the last evaluation so a re-eval runs after the tuned re-index even
	// when no evalSchedule is set; this lets auto-tune iterate up to maxAttempts.
	kb.Status.Evaluation = nil
}

// recordBest snapshots the effective chunking and recall when nothing has been
// recorded yet (first observation) or the just-run evaluation strictly beat the
// best recall seen so far. Ties keep the earlier (cheaper, larger-chunk) config.
// This lets auto-tune later revert to the best configuration rather than the last
// ladder step.
func recordBest(kb *ragv1alpha1.KnowledgeBase, recall int) {
	if kb.Status.BestChunking == nil || recall > kb.Status.BestRecallPercent {
		eff := effectiveChunking(kb)
		kb.Status.BestChunking = &eff
		kb.Status.BestRecallPercent = recall
	}
}

// settleOnBest reverts effective chunking to the best-observed config and forces
// one final re-index, unless the KB is already on it. Returns whether a revert
// was scheduled.
// settleOnBest reverts effective chunking to the best-observed config and forces
// one final re-index, unless the KB is already on it. Returns whether a revert
// was scheduled.
//
// Note: the caller is responsible for calling recordAutoTuneDuration
// before settleOnBest so the histogram sees the full run duration.
// settleOnBest no longer clears AutoTuneStartedAt directly.
func settleOnBest(kb *ragv1alpha1.KnowledgeBase) bool {
	if kb.Status.BestChunking == nil || effectiveChunking(kb) == *kb.Status.BestChunking {
		return false
	}
	best := *kb.Status.BestChunking
	kb.Status.EffectiveChunking = &best
	kb.Status.PendingRetune = true // owes a re-ingest on the next pass
	kb.Status.Evaluation = nil     // forces a re-eval after the re-index
	return true
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

func toSourceStatus(in []ragv1alpha1.IngestSourceResult) []ragv1alpha1.SourceStatus {
	out := make([]ragv1alpha1.SourceStatus, 0, len(in))
	for _, s := range in {
		out = append(out, ragv1alpha1.SourceStatus(s))
	}
	return out
}

// recordAutoTuneDuration observes the wall-clock duration of an
// auto-tune run and clears `AutoTuneStartedAt`. Called whenever a
// run ends (converged, exhausted, or reset by a user spec edit).
// Safe to call when `started` is nil; it's a no-op in that case.
func recordAutoTuneDuration(kb *ragv1alpha1.KnowledgeBase, result string, now time.Time) {
	if kb.Status.AutoTuneStartedAt == nil || kb.Status.AutoTuneStartedAt.IsZero() {
		return
	}
	dur := now.Sub(kb.Status.AutoTuneStartedAt.Time).Seconds()
	if dur > 0 {
		autoTuneDurationSeconds.WithLabelValues(kb.Namespace, result).Observe(dur)
	}
	kb.Status.AutoTuneStartedAt = nil
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

// isActiveJobTimedOut reports whether an in-flight Job has exceeded its
// own ActiveDeadlineSeconds. The trigger is a defensive cleanup for
// the case where the operator lost a watch event (restart, leader
// handoff) and the cluster didn't GC the Job (e.g. it was never
// created, or its controller owner is gone).
//
// `started` is the time the operator recorded when it launched the
// Job. `now` is injected for unit-testability. A 10-minute grace
// beyond the deadline absorbs clock skew and pod-graceful-shutdown
// time.
func isActiveJobTimedOut(j *batchv1.Job, started *metav1.Time, now time.Time) bool {
	if started == nil || started.IsZero() {
		return false // we don't know when it started; trust the standard
		// completion / failure detection in reconcileActiveJob.
	}
	deadline := int64(7200)
	if j.Spec.ActiveDeadlineSeconds != nil {
		deadline = *j.Spec.ActiveDeadlineSeconds
	}
	cutoff := started.Add(time.Duration(deadline) * time.Second).Add(10 * time.Minute)
	return now.After(cutoff)
}

func jobSpecHash(j *batchv1.Job, fallback string) string {
	if j.Labels != nil && j.Labels[labelSpecHash] != "" {
		return j.Labels[labelSpecHash]
	}
	return fallback
}

func jobSecretsHash(j *batchv1.Job, fallback string) string {
	if j.Labels != nil && j.Labels[labelSecretsHash] != "" {
		return j.Labels[labelSecretsHash]
	}
	return fallback
}

func activeIngestIsStale(job *batchv1.Job, desiredHash, desiredSecretsHash, observedHash, observedSecretsHash string) bool {
	if jobType(job) != jobTypeIngest {
		return false
	}
	return jobSpecHash(job, observedHash) != desiredHash ||
		jobSecretsHash(job, observedSecretsHash) != desiredSecretsHash
}

func ingestFailureRetryAfter(kb *ragv1alpha1.KnowledgeBase, desiredHash string, now time.Time) time.Duration {
	if kb.Status.Phase != ragv1alpha1.PhaseFailed ||
		kb.Status.LastFailedSpecHash != desiredHash ||
		kb.Status.LastFailureTime == nil {
		return 0
	}
	remaining := kb.Status.LastFailureTime.Add(ingestFailureRetryDelay).Sub(now)
	if remaining > 0 {
		return remaining
	}
	return 0
}

func jobEffectiveChunking(j *batchv1.Job, fallback ragv1alpha1.ChunkingSpec) ragv1alpha1.ChunkingSpec {
	if j.Labels == nil || j.Labels[labelChunking] == "" {
		return fallback
	}
	var c ragv1alpha1.ChunkingSpec
	parts := strings.SplitN(j.Labels[labelChunking], ".", 3)
	if len(parts) == 3 {
		c.Strategy = ragv1alpha1.ChunkingStrategy(parts[0])
		_, _ = fmt.Sscanf(parts[1], "%d", &c.MaxTokens)
		_, _ = fmt.Sscanf(parts[2], "%d", &c.Overlap)
	}
	if c.Strategy == "" {
		return fallback
	}
	return c
}

// chunkingLabel serialises a chunking spec to a compact label value.
func chunkingLabel(c ragv1alpha1.ChunkingSpec) string {
	return fmt.Sprintf("%s.%d.%d", c.Strategy, c.MaxTokens, c.Overlap)
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

func isQuotaExceeded(err error) bool {
	if err == nil {
		return false
	}
	return apierrors.IsForbidden(err) &&
		(strings.Contains(err.Error(), "exceeded quota") ||
			strings.Contains(err.Error(), "exceeded limitrange"))
}

func specConfigTooLargeUnchanged(kb *ragv1alpha1.KnowledgeBase) bool {
	condition := meta.FindStatusCondition(kb.Status.Conditions, ragv1alpha1.ConditionReady)
	return condition != nil &&
		condition.Reason == "SpecConfigTooLarge" &&
		condition.ObservedGeneration == kb.Generation
}

func (r *KnowledgeBaseReconciler) computeSecretsHash(ctx context.Context, kb *ragv1alpha1.KnowledgeBase) string {
	hasher := sha256.New()

	appendSecretHash(ctx, r.Client, kb.Namespace, "vectorStore.credentials", kb.Spec.VectorStore.CredentialsSecretRef, hasher)
	appendSecretHash(ctx, r.Client, kb.Namespace, "embedding.apiKey", kb.Spec.Embedding.APIKeySecretRef, hasher)

	for _, s := range kb.Spec.Sources {
		if s.GitHub != nil && s.GitHub.TokenSecretRef != nil {
			appendSecretHash(ctx, r.Client, kb.Namespace, "source.github."+s.Name, s.GitHub.TokenSecretRef, hasher)
		}
		if s.S3 != nil {
			appendSecretHash(ctx, r.Client, kb.Namespace, "source.s3.access."+s.Name, s.S3.AccessKeySecretRef, hasher)
			appendSecretHash(ctx, r.Client, kb.Namespace, "source.s3.secret."+s.Name, s.S3.SecretKeySecretRef, hasher)
		}
	}

	return hex.EncodeToString(hasher.Sum(nil)[:4])
}
