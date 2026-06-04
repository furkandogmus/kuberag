package controller

import (
	"context"
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

func TestSpecHashStableAndSensitive(t *testing.T) {
	kb := baseKB()
	h1 := specHash(kb, "")
	if h1 != specHash(kb, "") {
		t.Fatal("specHash not deterministic")
	}

	// Changing the embedding model must change the hash.
	kb.Spec.Embedding.Model = "bge-large"
	if specHash(kb, "") == h1 {
		t.Fatal("specHash should change when model changes")
	}

	// Changing spec chunking must change the hash.
	kb2 := baseKB()
	kb2.Spec.Chunking.MaxTokens = 500
	if specHash(kb2, "") == h1 {
		t.Fatal("specHash should change when chunking changes")
	}

	// An auto-tuned override (status) must NOT change the hash — auto-tune
	// forces re-ingest by clearing ObservedSpecHash, not via the hash.
	kb3 := baseKB()
	tuned := specChunking(kb3)
	tuned.Overlap += 40
	kb3.Status.EffectiveChunking = &tuned
	if specHash(kb3, "") != h1 {
		t.Fatal("specHash must ignore the auto-tune override")
	}
}

func TestNeedsIngest(t *testing.T) {
	kb := baseKB()
	hash := specHash(kb, "")

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
	newHash := specHash(kb, "")
	if _, need := needsIngest(kb, newHash); !need {
		t.Fatal("model change should trigger ingest")
	}
}

func TestNeedsIngestFreshness(t *testing.T) {
	kb := baseKB()
	kb.Spec.Freshness.Schedule = "*/5 * * * *" // every 5 minutes
	hash := specHash(kb, "")
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

func TestSecurityContextHardening(t *testing.T) {
	kb := baseKB()
	// Test baseJob security context and volumes
	job := baseJob(kb, "test-job", "ingest", "hash123", []string{"ingest"}, nil)
	if job == nil {
		t.Fatal("expected non-nil Job")
	}

	podSpec := job.Spec.Template.Spec
	if podSpec.SecurityContext == nil || podSpec.SecurityContext.RunAsNonRoot == nil || !*podSpec.SecurityContext.RunAsNonRoot {
		t.Error("expected Job pod security context with RunAsNonRoot=true")
	}
	if len(podSpec.Volumes) != 1 || podSpec.Volumes[0].Name != "scratch" {
		t.Error("expected Job pod to have 'scratch' volume")
	}

	container := podSpec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("expected Job container security context with AllowPrivilegeEscalation=false and ReadOnlyRootFilesystem=true")
	}
	if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].Name != "scratch" || container.VolumeMounts[0].MountPath != "/scratch" {
		t.Error("expected Job container to have 'scratch' volume mount at /scratch")
	}

	// Verify env vars
	var hasHome bool
	for _, env := range container.Env {
		if env.Name == "HOME" && env.Value == "/scratch" {
			hasHome = true
		}
	}
	if !hasHome {
		t.Error("expected Job container to have HOME=/scratch environment variable")
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

	job := baseJob(kb, "test-job", "ingest", "hash123", []string{"ingest"}, nil)
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
	specHash1 := specHash(kb, hash1)

	// Change secret value and verify both secrets hash and spec hash change
	secret1.Data["token"] = []byte("git-token-value-updated")
	fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret1).Build()
	r.Client = fakeClient

	hash2 := r.computeSecretsHash(context.Background(), kb)
	specHash2 := specHash(kb, hash2)

	if hash1 == hash2 {
		t.Error("expected KB secrets hash to change when source secret is updated")
	}
	if specHash1 == specHash2 {
		t.Error("expected KB spec hash to change when source secret is updated")
	}
}
