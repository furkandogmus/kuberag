package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

func TestSpecHashStableAndSensitive(t *testing.T) {
	kb := baseKB()
	eff := effectiveChunking(kb)
	h1 := specHash(kb, eff)
	if h1 != specHash(kb, eff) {
		t.Fatal("specHash not deterministic")
	}

	// Changing the embedding model must change the hash.
	kb.Spec.Embedding.Model = "bge-large"
	if specHash(kb, effectiveChunking(kb)) == h1 {
		t.Fatal("specHash should change when model changes")
	}

	// Changing chunking must change the hash.
	kb2 := baseKB()
	eff2 := effectiveChunking(kb2)
	eff2.MaxTokens = 500
	if specHash(kb2, eff2) == h1 {
		t.Fatal("specHash should change when chunking changes")
	}
}

func TestNeedsIngest(t *testing.T) {
	kb := baseKB()
	eff := effectiveChunking(kb)
	hash := specHash(kb, eff)

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
	newHash := specHash(kb, effectiveChunking(kb))
	if _, need := needsIngest(kb, newHash); !need {
		t.Fatal("model change should trigger ingest")
	}
}

func TestNeedsIngestFreshness(t *testing.T) {
	kb := baseKB()
	kb.Spec.Freshness.Schedule = "*/5 * * * *" // every 5 minutes
	eff := effectiveChunking(kb)
	hash := specHash(kb, eff)
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
	if kb.Status.ObservedSpecHash != "" {
		t.Fatal("auto-tune must clear ObservedSpecHash to force re-index")
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
