package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

func boolPtr(b bool) *bool { return &b }

func baseKB() *ragv1alpha1.KnowledgeBase {
	return &ragv1alpha1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb", Namespace: "default"},
		Spec: ragv1alpha1.KnowledgeBaseSpec{
			Sources: []ragv1alpha1.Source{{
				Name: "docs", Type: ragv1alpha1.SourceGitHub,
				GitHub: &ragv1alpha1.GitHubSource{Repo: "org/docs"},
			}},
			Embedding:   ragv1alpha1.EmbeddingSpec{Model: "bge-small", Provider: "local"},
			VectorStore: ragv1alpha1.VectorStoreSpec{Type: ragv1alpha1.VectorStoreQdrant, Endpoint: "http://q:6333"},
		},
	}
}

func TestEffectiveChunkingDefaults(t *testing.T) {
	kb := baseKB()
	eff := effectiveChunking(kb)
	if eff.Strategy != ragv1alpha1.ChunkSemantic || eff.MaxTokens != 800 || eff.Overlap != 80 {
		t.Fatalf("unexpected defaults: %+v", eff)
	}

	tuned := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkFixed, MaxTokens: 400, Overlap: 120}
	kb.Status.EffectiveChunking = &tuned
	if got := effectiveChunking(kb); got != tuned {
		t.Fatalf("expected tuned chunking %+v, got %+v", tuned, got)
	}
}

func TestCorpusHashStableAndSensitive(t *testing.T) {
	kb := baseKB()
	h1 := corpusHash(kb)
	if h1 != corpusHash(kb) {
		t.Fatal("corpusHash not deterministic")
	}

	// Changing the embedding model must change the hash.
	kb.Spec.Embedding.Model = "bge-large"
	if corpusHash(kb) == h1 {
		t.Fatal("corpusHash should change when model changes")
	}

	// Changing spec chunking must change the hash.
	kb2 := baseKB()
	kb2.Spec.Chunking.MaxTokens = 500
	if corpusHash(kb2) == h1 {
		t.Fatal("corpusHash should change when chunking changes")
	}

	// Changing embedding provider details must change the hash even when the
	// model name stays the same.
	kbProvider := baseKB()
	kbProvider.Spec.Embedding.Provider = "openai-compatible"
	kbProvider.Spec.Embedding.BaseURL = "http://embeddings:8080/v1"
	if corpusHash(kbProvider) == h1 {
		t.Fatal("corpusHash should change when embedding provider details change")
	}

	// Changing the target collection must trigger re-ingestion into the new store location.
	kbStore := baseKB()
	kbStore.Spec.VectorStore.Collection = "other-collection"
	if corpusHash(kbStore) == h1 {
		t.Fatal("corpusHash should change when vector store collection changes")
	}

	// An auto-tuned override (status) must NOT change the hash — auto-tune
	// forces re-ingest via PendingRetune, not via the hash.
	kb3 := baseKB()
	tuned := specChunking(kb3)
	tuned.Overlap += 40
	kb3.Status.EffectiveChunking = &tuned
	if corpusHash(kb3) != h1 {
		t.Fatal("corpusHash must ignore the auto-tune override")
	}

	// Secret *references* (name/key) are part of the spec and DO change the hash.
	// Only the secret *values* (from computeSecretsHash) are excluded — credential
	// rotation should not trigger re-indexing (tested in TestKBSecretsHashAndWatch).
}

func TestCompletedJobCarriesOwnHashAndChunking(t *testing.T) {
	kb := baseKB()
	jobEff := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkFixed, MaxTokens: 500, Overlap: 50}
	job, _, err := buildIngestJob(kb, "oldhash", "oldsecrets", ragv1alpha1.IngestFull, jobEff)
	if err != nil {
		t.Fatalf("buildIngestJob returned error: %v", err)
	}

	currentEff := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkSemantic, MaxTokens: 900, Overlap: 90}
	if got := jobSpecHash(job, "newhash"); got != "oldhash" {
		t.Fatalf("expected completed job hash oldhash, got %q", got)
	}
	if got := jobSecretsHash(job, "newsecrets"); got != "oldsecrets" {
		t.Fatalf("expected completed job secrets hash oldsecrets, got %q", got)
	}
	if got := jobEffectiveChunking(job, currentEff); got != jobEff {
		t.Fatalf("expected job chunking %+v, got %+v", jobEff, got)
	}
}

func TestActiveIngestStalenessUsesJobHash(t *testing.T) {
	kb := baseKB()
	currentHash := corpusHash(kb)
	job, _, err := buildIngestJob(kb, currentHash, "s1", ragv1alpha1.IngestFull, effectiveChunking(kb))
	if err != nil {
		t.Fatalf("buildIngestJob returned error: %v", err)
	}

	if activeIngestIsStale(job, currentHash, "s1", "previous-hash", "s1") {
		t.Fatal("new ingest for the desired spec must not be cancelled because the last completed hash is old")
	}
	job.Labels[labelSpecHash] = "previous-hash"
	if !activeIngestIsStale(job, currentHash, "s1", "previous-hash", "s1") {
		t.Fatal("ingest created for an older spec should be cancelled")
	}

	// Changing secrets should also mark the job as stale.
	job2, _, err := buildIngestJob(kb, currentHash, "s2", ragv1alpha1.IngestFull, effectiveChunking(kb))
	if err != nil {
		t.Fatalf("buildIngestJob returned error: %v", err)
	}
	job2.Labels[labelSecretsHash] = "old-secrets"
	if !activeIngestIsStale(job2, currentHash, "new-secrets", currentHash, "old-secrets") {
		t.Fatal("ingest with outdated secrets should be cancelled")
	}
}

func TestIngestFailureRetryCooldown(t *testing.T) {
	kb := baseKB()
	hash := corpusHash(kb)
	now := time.Now()
	failedAt := metav1.NewTime(now.Add(-time.Minute))
	kb.Status.Phase = ragv1alpha1.PhaseFailed
	kb.Status.LastFailedSpecHash = hash
	kb.Status.LastFailureTime = &failedAt

	if got := ingestFailureRetryAfter(kb, hash, now); got != 4*time.Minute {
		t.Fatalf("retryAfter=%s, want 4m", got)
	}
	if got := ingestFailureRetryAfter(kb, "changed-spec", now); got != 0 {
		t.Fatalf("spec change should bypass cooldown, got %s", got)
	}
	if got := ingestFailureRetryAfter(kb, hash, now.Add(5*time.Minute)); got != 0 {
		t.Fatalf("expired cooldown should allow retry, got %s", got)
	}
}

func TestNeedsIngest(t *testing.T) {
	kb := baseKB()
	hash := corpusHash(kb)

	if _, need := needsIngest(kb, hash); !need {
		t.Fatal("fresh KB with no observed hash should need ingest")
	}

	// Up to date.
	kb.Status.ObservedSpecHash = hash
	kb.Status.ObservedEmbeddingModel = "bge-small"
	if _, need := needsIngest(kb, hash); need {
		t.Fatal("up-to-date KB should not need ingest")
	}

	// Model drift.
	kb.Spec.Embedding.Model = "bge-large"
	newHash := corpusHash(kb)
	if _, need := needsIngest(kb, newHash); !need {
		t.Fatal("model change should trigger ingest")
	}
}

func TestNeedsIngestFreshness(t *testing.T) {
	kb := baseKB()
	kb.Spec.Freshness.Schedule = "*/5 * * * *" // every 5 minutes
	hash := corpusHash(kb)
	kb.Status.ObservedSpecHash = hash
	kb.Status.ObservedEmbeddingModel = "bge-small"

	old := metav1.NewTime(time.Now().Add(-30 * time.Minute))
	kb.Status.LastIndexedTime = &old
	if _, need := needsIngest(kb, hash); !need {
		t.Fatal("stale freshness window should trigger ingest")
	}

	recent := metav1.NewTime(time.Now())
	kb.Status.LastIndexedTime = &recent
	if _, need := needsIngest(kb, hash); need {
		t.Fatal("recently indexed KB should not be due yet")
	}
}

func TestCronDue(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 30, 0, 0, time.UTC)
	// Last ran an hour ago (the 12:00 fire has since passed) -> due.
	if !cronDue("0 * * * *", now.Add(-time.Hour), now) {
		t.Fatal("hourly schedule should be due after an hour")
	}
	// Last ran a minute ago; next fire is 13:00 which is in the future -> not due.
	if cronDue("0 * * * *", now.Add(-time.Minute), now) {
		t.Fatal("hourly schedule should not be due after a minute")
	}
	// Empty schedule is never due.
	if cronDue("", time.Time{}, now) {
		t.Fatal("empty schedule should never be due")
	}
}

func TestApplyAutoTune(t *testing.T) {
	kb := baseKB()
	kb.Status.ObservedSpecHash = "abc"
	before := effectiveChunking(kb)

	applyAutoTune(kb)
	after := *kb.Status.EffectiveChunking
	if after.Overlap <= before.Overlap && after.MaxTokens >= before.MaxTokens {
		t.Fatalf("auto-tune should change chunking: before %+v after %+v", before, after)
	}
	if kb.Status.AutoTuneAttempts != 1 {
		t.Fatalf("expected attempts=1, got %d", kb.Status.AutoTuneAttempts)
	}
	if !kb.Status.PendingRetune {
		t.Fatal("auto-tune must set PendingRetune to force re-index")
	}
	// ObservedSpecHash must be left intact so a user spec edit is still detected
	// while the tuned re-index is pending.
	if kb.Status.ObservedSpecHash != "abc" {
		t.Fatalf("auto-tune must not disturb ObservedSpecHash, got %q", kb.Status.ObservedSpecHash)
	}
}

func TestUserEditedSpec(t *testing.T) {
	tuned := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkSemantic, MaxTokens: 600, Overlap: 120}

	// No override active -> never a drop, whatever the hash.
	kb := baseKB()
	kb.Status.ObservedSpecHash = "h1"
	if userEditedSpec(kb, "h2") {
		t.Fatal("no override should never be dropped")
	}

	// Override active, hash unchanged -> keep (this is the steady tuning case).
	kb.Status.EffectiveChunking = &tuned
	kb.Status.PendingRetune = true // mid-tune: re-index pending
	if userEditedSpec(kb, "h1") {
		t.Fatal("unedited spec must keep the override even mid-tune")
	}

	// Override active and the user edited the spec (hash drift) -> drop, even while
	// a tuned re-index is still pending. This is the regression guard: ObservedSpecHash
	// is no longer cleared by auto-tune, so the edit is detected instead of masked.
	if !userEditedSpec(kb, "h2") {
		t.Fatal("a spec edit during a pending retune must drop the override")
	}

	// Never-ingested KB (empty hash) with no override -> not a drop.
	fresh := baseKB()
	if userEditedSpec(fresh, "h2") {
		t.Fatal("never-ingested KB should not report a spec edit")
	}
}

func TestNextChunking(t *testing.T) {
	// Early steps grow overlap toward maxTokens/2.
	c := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkSemantic, MaxTokens: 800, Overlap: 80}
	c = nextChunking(c)
	if c.Overlap != 120 || c.MaxTokens != 800 || c.Strategy != ragv1alpha1.ChunkSemantic {
		t.Fatalf("first step should grow overlap only: %+v", c)
	}

	// Drive the ladder to the floor; maxTokens must never breach chunkFloor and
	// the strategy must eventually rotate to a different boundary model.
	c = ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkSemantic, MaxTokens: 800, Overlap: 80}
	rotated := false
	for i := 0; i < 50; i++ {
		c = nextChunking(c)
		if c.MaxTokens < chunkFloor {
			t.Fatalf("maxTokens %d breached floor %d at step %d", c.MaxTokens, chunkFloor, i)
		}
		if c.Strategy != ragv1alpha1.ChunkSemantic {
			rotated = true
			break
		}
	}
	if !rotated {
		t.Fatal("ladder never rotated strategy after exhausting chunk size")
	}

	// Strategy rotation cycles semantic -> recursive -> fixed -> semantic.
	if got := rotateStrategy(ragv1alpha1.ChunkSemantic); got != ragv1alpha1.ChunkRecursive {
		t.Fatalf("semantic should rotate to recursive, got %q", got)
	}
	if got := rotateStrategy(ragv1alpha1.ChunkRecursive); got != ragv1alpha1.ChunkFixed {
		t.Fatalf("recursive should rotate to fixed, got %q", got)
	}
	if got := rotateStrategy(ragv1alpha1.ChunkFixed); got != ragv1alpha1.ChunkSemantic {
		t.Fatalf("fixed should rotate back to semantic, got %q", got)
	}
}

func TestRecordBest(t *testing.T) {
	kb := baseKB()
	// First observation seeds the best, whatever the recall.
	recordBest(kb, 40)
	if kb.Status.BestChunking == nil || kb.Status.BestRecallPercent != 40 {
		t.Fatalf("first observation should seed best: %d %+v", kb.Status.BestRecallPercent, kb.Status.BestChunking)
	}
	seeded := *kb.Status.BestChunking

	// A better recall on a different config updates both.
	tuned := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkSemantic, MaxTokens: 600, Overlap: 120}
	kb.Status.EffectiveChunking = &tuned
	recordBest(kb, 70)
	if kb.Status.BestRecallPercent != 70 || *kb.Status.BestChunking != tuned {
		t.Fatalf("better recall should update best: %d %+v", kb.Status.BestRecallPercent, kb.Status.BestChunking)
	}

	// A regression must not overwrite the best.
	worse := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkFixed, MaxTokens: 300, Overlap: 60}
	kb.Status.EffectiveChunking = &worse
	recordBest(kb, 55)
	if kb.Status.BestRecallPercent != 70 || *kb.Status.BestChunking != tuned {
		t.Fatalf("regression must not overwrite best: %d %+v", kb.Status.BestRecallPercent, kb.Status.BestChunking)
	}
	_ = seeded
}

func TestSettleOnBest(t *testing.T) {
	// No best recorded -> nothing to settle on.
	kb := baseKB()
	if settleOnBest(kb) {
		t.Fatal("settleOnBest should be a no-op without a recorded best")
	}

	// Best differs from current -> revert and force a re-index + re-eval.
	best := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkSemantic, MaxTokens: 600, Overlap: 120}
	kb.Status.BestChunking = &best
	kb.Status.BestRecallPercent = 70
	current := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkFixed, MaxTokens: 300, Overlap: 60}
	kb.Status.EffectiveChunking = &current
	kb.Status.ObservedSpecHash = "abc"
	now := metav1.Now()
	kb.Status.Evaluation = &ragv1alpha1.EvaluationStatus{Time: &now}
	if !settleOnBest(kb) {
		t.Fatal("settleOnBest should revert when current differs from best")
	}
	if *kb.Status.EffectiveChunking != best {
		t.Fatalf("effective chunking should be reverted to best: %+v", kb.Status.EffectiveChunking)
	}
	if !kb.Status.PendingRetune || kb.Status.Evaluation != nil {
		t.Fatal("settleOnBest must mark PendingRetune and clear evaluation to force re-index + re-eval")
	}
	// ObservedSpecHash stays intact so a concurrent spec edit is still detectable.
	if kb.Status.ObservedSpecHash != "abc" {
		t.Fatalf("settleOnBest must not disturb ObservedSpecHash, got %q", kb.Status.ObservedSpecHash)
	}

	// Already on best -> no-op (this is how the settle loop terminates).
	if settleOnBest(kb) {
		t.Fatal("settleOnBest should be a no-op once already on best")
	}
}

func TestChunkFingerprint(t *testing.T) {
	a := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkSemantic, MaxTokens: 800, Overlap: 80}
	aCopy := a
	b := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkSemantic, MaxTokens: 800, Overlap: 120}
	// Stable for equal configs, distinct for different ones — this is what keeps a
	// settle/revert re-index from colliding with the prior attempt's Job name.
	if chunkFingerprint(a) != chunkFingerprint(aCopy) {
		t.Fatal("fingerprint must be stable for equal chunking")
	}
	if chunkFingerprint(a) == chunkFingerprint(b) {
		t.Fatal("fingerprint must differ for different chunking")
	}
	// Strategy alone must change it (same sizes, different boundary model).
	c := ragv1alpha1.ChunkingSpec{Strategy: ragv1alpha1.ChunkFixed, MaxTokens: 800, Overlap: 80}
	if chunkFingerprint(a) == chunkFingerprint(c) {
		t.Fatal("fingerprint must reflect the chunking strategy")
	}
}

func TestEvalDue(t *testing.T) {
	kb := baseKB()
	// No retrievalQuality -> never.
	if evalDue(kb) {
		t.Fatal("eval should not be due without retrievalQuality")
	}

	kb.Spec.RetrievalQuality = &ragv1alpha1.RetrievalQualitySpec{
		Enabled:    boolPtr(true),
		DatasetRef: ragv1alpha1.LocalObjectRef{Name: "ds"},
	}
	// Not ready / no chunks -> not due.
	if evalDue(kb) {
		t.Fatal("eval should not be due before data exists")
	}

	kb.Status.Phase = ragv1alpha1.PhaseReady
	kb.Status.IndexedChunks = 100
	// Never evaluated -> due.
	if !evalDue(kb) {
		t.Fatal("eval should be due on first run once ready")
	}

	// Evaluated, no schedule -> not due again.
	now := metav1.Now()
	kb.Status.Evaluation = &ragv1alpha1.EvaluationStatus{Time: &now}
	if evalDue(kb) {
		t.Fatal("eval without schedule should not repeat")
	}
}

func TestEmbeddingDimension(t *testing.T) {
	cases := []struct {
		spec ragv1alpha1.EmbeddingSpec
		want int
	}{
		{ragv1alpha1.EmbeddingSpec{Model: "bge-small", Provider: "local"}, 384},
		{ragv1alpha1.EmbeddingSpec{Model: "bge-large", Provider: "local"}, 1024},
		{ragv1alpha1.EmbeddingSpec{Model: "text-embedding-3-small", Provider: "openai"}, 1536},
		{ragv1alpha1.EmbeddingSpec{Model: "text-embedding-004", Provider: "gemini"}, 768},
		{ragv1alpha1.EmbeddingSpec{Model: "unknown-local", Provider: "local"}, 384},
		{ragv1alpha1.EmbeddingSpec{Model: "unknown-hosted", Provider: "openai-compatible"}, 0},
		{ragv1alpha1.EmbeddingSpec{Model: "custom", Provider: "openai-compatible", Dimension: 256}, 256},
	}
	for _, c := range cases {
		if got := embeddingDimension(c.spec); got != c.want {
			t.Errorf("embeddingDimension(%+v)=%d, want %d", c.spec, got, c.want)
		}
	}
}

func TestAutoTuneHelpers(t *testing.T) {
	if autoTuneEnabled(nil) {
		t.Fatal("nil retrievalQuality should not enable auto-tune")
	}
	rq := &ragv1alpha1.RetrievalQualitySpec{AutoTune: &ragv1alpha1.AutoTuneSpec{Enabled: boolPtr(true)}}
	if !autoTuneEnabled(rq) {
		t.Fatal("auto-tune should be enabled")
	}
	if autoTuneMax(rq) != 3 {
		t.Fatalf("default max attempts should be 3, got %d", autoTuneMax(rq))
	}
	rq.AutoTune.MaxAttempts = 5
	if autoTuneMax(rq) != 5 {
		t.Fatalf("max attempts should be 5, got %d", autoTuneMax(rq))
	}
}

func TestSecurityContextHardening(t *testing.T) {
	kb := baseKB()
	// Test baseJob security context and volumes
	job, err := baseJob(kb, "test-job", "ingest", "hash123", "secrets1", "test-job-spec", []string{"ingest"}, nil)
	if err != nil {
		t.Fatalf("baseJob returned error: %v", err)
	}
	if job == nil {
		t.Fatal("expected non-nil Job")
	}

	podSpec := job.Spec.Template.Spec
	if podSpec.SecurityContext == nil || podSpec.SecurityContext.RunAsNonRoot == nil || !*podSpec.SecurityContext.RunAsNonRoot {
		t.Error("expected Job pod security context with RunAsNonRoot=true")
	}
	if len(podSpec.Volumes) != 2 {
		t.Errorf("expected 2 volumes (scratch + spec), got %d", len(podSpec.Volumes))
	}
	var hasScratchVol, hasSpecVol bool
	for _, v := range podSpec.Volumes {
		if v.Name == "scratch" {
			hasScratchVol = true
		}
		if v.Name == "spec" {
			hasSpecVol = true
		}
	}
	if !hasScratchVol {
		t.Error("expected Job pod to have 'scratch' volume")
	}
	if !hasSpecVol {
		t.Error("expected Job pod to have 'spec' ConfigMap volume")
	}

	container := podSpec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("expected Job container security context with AllowPrivilegeEscalation=false and ReadOnlyRootFilesystem=true")
	}
	if len(container.VolumeMounts) != 2 {
		t.Errorf("expected 2 volume mounts (scratch + spec), got %d", len(container.VolumeMounts))
	}
	var hasScratchMount, hasSpecMount bool
	for _, m := range container.VolumeMounts {
		if m.Name == "scratch" && m.MountPath == "/scratch" {
			hasScratchMount = true
		}
		if m.Name == "spec" && m.MountPath == "/etc/kuberag" {
			hasSpecMount = true
		}
	}
	if !hasScratchMount {
		t.Error("expected Job container to have 'scratch' volume mount at /scratch")
	}
	if !hasSpecMount {
		t.Error("expected Job container to have 'spec' volume mount at /etc/kuberag")
	}

	// Verify env vars
	var hasHome, hasSpecPath bool
	for _, env := range container.Env {
		if env.Name == "HOME" && env.Value == "/scratch" {
			hasHome = true
		}
		if env.Name == "KB_SPEC_PATH" && env.Value == "/etc/kuberag/spec.json" {
			hasSpecPath = true
		}
	}
	if !hasHome {
		t.Error("expected Job container to have HOME=/scratch environment variable")
	}
	if !hasSpecPath {
		t.Error("expected Job container to have KB_SPEC_PATH=/etc/kuberag/spec.json environment variable")
	}

	// Test desiredDeployment security context and volumes
	rt := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "test-rt", Namespace: "default"},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "kb"},
			Replicas:         1,
		},
	}
	r := &RetrieverReconciler{}
	dep := r.desiredDeployment(rt, kb, "test-hash-value")
	if dep == nil {
		t.Fatal("expected non-nil Deployment")
	}

	depPodSpec := dep.Spec.Template.Spec
	if depPodSpec.SecurityContext == nil || depPodSpec.SecurityContext.RunAsNonRoot == nil || !*depPodSpec.SecurityContext.RunAsNonRoot {
		t.Error("expected Deployment pod security context with RunAsNonRoot=true")
	}
	if depPodSpec.AutomountServiceAccountToken == nil || *depPodSpec.AutomountServiceAccountToken {
		t.Error("expected retriever Deployment to disable ServiceAccount token automount")
	}
	if len(depPodSpec.Volumes) != 1 || depPodSpec.Volumes[0].Name != "scratch" {
		t.Error("expected Deployment pod to have 'scratch' volume")
	}

	depContainer := depPodSpec.Containers[0]
	if depContainer.SecurityContext == nil || depContainer.SecurityContext.AllowPrivilegeEscalation == nil || *depContainer.SecurityContext.AllowPrivilegeEscalation || depContainer.SecurityContext.ReadOnlyRootFilesystem == nil || !*depContainer.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("expected Deployment container security context with AllowPrivilegeEscalation=false and ReadOnlyRootFilesystem=true")
	}
	if len(depContainer.VolumeMounts) != 1 || depContainer.VolumeMounts[0].Name != "scratch" || depContainer.VolumeMounts[0].MountPath != "/scratch" {
		t.Error("expected Deployment container to have 'scratch' volume mount at /scratch")
	}

	var depHasHome bool
	for _, env := range depContainer.Env {
		if env.Name == "HOME" && env.Value == "/scratch" {
			depHasHome = true
		}
	}
	if !depHasHome {
		t.Error("expected Deployment container to have HOME=/scratch environment variable")
	}
}

func TestDeploymentSchedulingAndChecksums(t *testing.T) {
	// 1. Test scheduling mapping for retriever deployment
	kb := baseKB()
	rt := &ragv1alpha1.Retriever{
		ObjectMeta: metav1.ObjectMeta{Name: "test-rt", Namespace: "default"},
		Spec: ragv1alpha1.RetrieverSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "kb"},
			Replicas:         1,
			NodeSelector:     map[string]string{"kubernetes.io/hostname": "gpu-node"},
			Tolerations: []corev1.Toleration{
				{Key: "gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			},
		},
	}
	r := &RetrieverReconciler{}
	dep := r.desiredDeployment(rt, kb, "some-hash")
	if dep == nil {
		t.Fatal("expected non-nil Deployment")
	}

	depPodSpec := dep.Spec.Template.Spec
	if depPodSpec.NodeSelector["kubernetes.io/hostname"] != "gpu-node" {
		t.Error("expected NodeSelector to be mapped")
	}
	if len(depPodSpec.Tolerations) != 1 || depPodSpec.Tolerations[0].Key != "gpu" {
		t.Error("expected Tolerations to be mapped")
	}
	if dep.Spec.Template.Annotations["checksum/secrets"] != "some-hash" {
		t.Error("expected checksum annotation to be mapped")
	}

	// 2. Test scheduling mapping for baseJob
	kb.Spec.Ingestion.NodeSelector = map[string]string{"kubernetes.io/hostname": "cpu-node"}
	kb.Spec.Ingestion.Tolerations = []corev1.Toleration{
		{Key: "cpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}

	job, err := baseJob(kb, "test-job", "ingest", "hash123", "secrets1", "test-job-spec", []string{"ingest"}, nil)
	if err != nil {
		t.Fatalf("baseJob returned error: %v", err)
	}
	if job == nil {
		t.Fatal("expected non-nil Job")
	}
	jobPodSpec := job.Spec.Template.Spec
	if jobPodSpec.NodeSelector["kubernetes.io/hostname"] != "cpu-node" {
		t.Error("expected Job NodeSelector to be mapped")
	}
	if len(jobPodSpec.Tolerations) != 1 || jobPodSpec.Tolerations[0].Key != "cpu" {
		t.Error("expected Job Tolerations to be mapped")
	}

	// 3. Test computeSecretsHash with fake client
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = ragv1alpha1.AddToScheme(scheme)

	secret1 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vector-secret", Namespace: "default"},
		Data:       map[string][]byte{"apiKey": []byte("vector-pass")},
	}
	secret2 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gen-secret", Namespace: "default"},
		Data:       map[string][]byte{"apiKey": []byte("gen-pass")},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret1, secret2).Build()
	r.Client = fakeClient

	kb.Spec.VectorStore.CredentialsSecretRef = &ragv1alpha1.SecretKeyRef{
		Name: "vector-secret",
		Key:  "apiKey",
	}
	rt.Spec.Generation = &ragv1alpha1.GenerationSpec{
		Enabled:  boolPtr(true),
		Provider: "openai",
		Model:    "gpt-4o",
		APIKeySecretRef: &ragv1alpha1.SecretKeyRef{
			Name: "gen-secret",
			Key:  "apiKey",
		},
	}

	hash1 := r.computeSecretsHash(context.Background(), rt, kb)

	// Change secret value and verify hash changes
	secret1.Data["apiKey"] = []byte("vector-pass-new")
	fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret1, secret2).Build()
	r.Client = fakeClient

	hash2 := r.computeSecretsHash(context.Background(), rt, kb)
	if hash1 == hash2 {
		t.Error("expected secret checksum hash to change when secret data is updated")
	}
}

func TestKBSecretsHashAndWatch(t *testing.T) {
	kb := baseKB()
	r := &KnowledgeBaseReconciler{}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = ragv1alpha1.AddToScheme(scheme)

	secret1 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-secret", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("git-token-value")},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret1).Build()
	r.Client = fakeClient

	// Link git secret to the git source
	kb.Spec.Sources[0].GitHub.TokenSecretRef = &ragv1alpha1.SecretKeyRef{
		Name: "git-secret",
		Key:  "token",
	}

	hash1 := r.computeSecretsHash(context.Background(), kb)
	corpusHash1 := corpusHash(kb)

	// Change secret value — secretsHash must change, but corpusHash must NOT.
	secret1.Data["token"] = []byte("git-token-value-updated")
	fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret1).Build()
	r.Client = fakeClient

	hash2 := r.computeSecretsHash(context.Background(), kb)
	corpusHash2 := corpusHash(kb)

	if hash1 == hash2 {
		t.Error("expected secrets hash to change when source secret is updated")
	}
	if corpusHash1 != corpusHash2 {
		t.Error("corpusHash must NOT change when only secret values change — credential rotation should not trigger re-index")
	}
}

func TestInvalidIngestionResourcesReturnError(t *testing.T) {
	kb := baseKB()
	kb.Spec.Ingestion.Resources = &ragv1alpha1.ResourceRequirements{CPU: "not-a-quantity"}

	if _, _, err := buildIngestJob(kb, "hash123", "secrets1", ragv1alpha1.IngestFull, effectiveChunking(kb)); err == nil {
		t.Fatal("expected invalid ingestion resources to return an error")
	}
}

func TestConfigMapNamesPreserveSuffixWithinDNSLimit(t *testing.T) {
	jobName := strings.Repeat("a", 63)
	for _, got := range []string{resultConfigMapName(jobName), specConfigMapName(jobName)} {
		if len(got) > 63 {
			t.Fatalf("generated ConfigMap name exceeds DNS label limit: %q", got)
		}
		if !strings.HasSuffix(got, "-result") && !strings.HasSuffix(got, "-spec") {
			t.Fatalf("generated ConfigMap name lost suffix: %q", got)
		}
	}
}

func TestQdrantProbeUsesCredentialSecret(t *testing.T) {
	const apiKey = "qdrant-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("api-key"); got != apiKey {
			http.Error(w, fmt.Sprintf("unexpected api-key %q", got), http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"green","result":{"points_count":12,"config":{"params":{"vectors":{"size":384}}}}}`)
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "qdrant-auth", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte(apiKey)},
	}
	r := &VectorIndexReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		HTTP:   server.Client(),
	}
	vi := &ragv1alpha1.VectorIndex{
		ObjectMeta: metav1.ObjectMeta{Name: "docs-index", Namespace: "default"},
		Spec: ragv1alpha1.VectorIndexSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "docs"},
			Store: ragv1alpha1.VectorStoreSpec{
				Type:                 ragv1alpha1.VectorStoreQdrant,
				Endpoint:             server.URL,
				Collection:           "docs",
				CredentialsSecretRef: &ragv1alpha1.SecretKeyRef{Name: "qdrant-auth", Key: "api-key"},
			},
			Dimension: 384,
		},
	}

	got := r.probeQdrant(context.Background(), vi)
	if got.health != ragv1alpha1.IndexHealthy || got.points != 12 || got.dimension != 384 {
		t.Fatalf("unexpected authenticated Qdrant probe result: %+v", got)
	}
}
