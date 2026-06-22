package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

const (
	defaultWorkerImage    = "ghcr.io/furkandogmus/kuberag-worker:latest"
	defaultRetrieverImage = "ghcr.io/furkandogmus/kuberag-retriever:latest"

	maxConfigMapBytes = 950 * 1024

	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelKB          = "rag.furkan.dev/knowledgebase"
	labelJobType     = "rag.furkan.dev/job-type"
	labelSpecHash    = "rag.furkan.dev/spec-hash"
	labelSecretsHash = "rag.furkan.dev/secrets-hash"
	labelChunking    = "rag.furkan.dev/chunking" // strategy|maxTokens|overlap for jobEffectiveChunking

	jobTypeIngest  = "ingest"
	jobTypeEval    = "eval"
	jobTypeCleanup = "cleanup"
)

// jobType reads the job-type label from a Job.
func jobType(j *batchv1.Job) string { return j.Labels[labelJobType] }

// resultConfigMapName is where the worker writes its structured result.
func resultConfigMapName(jobName string) string { return nameWithSuffix(jobName, "-result") }

// checkpointConfigMapName is where the worker writes its incremental checkpoint
// so interrupted ingestion can be resumed by a subsequent Job.
func checkpointConfigMapName(jobName string) string { return nameWithSuffix(jobName, "-checkpoint") }

// IngestResult is the JSON the ingestion worker writes to its result ConfigMap.
type IngestResult struct {
	TotalChunks int                              `json:"totalChunks"`
	Sources     []ragv1alpha1.IngestSourceResult `json:"sources"`
}

// EvalResult is the JSON the eval worker writes.
type EvalResult struct {
	RecallPercent    int `json:"recallPercent"`
	P95LatencyMillis int `json:"p95LatencyMillis"`
	Queries          int `json:"queries"`
}

// readResultConfigMap fetches and parses the worker result into out.
func (r *KnowledgeBaseReconciler) readResult(ctx context.Context, ns, jobName string, out any) error {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: ns, Name: resultConfigMapName(jobName)}
	if err := r.Get(ctx, key, &cm); err != nil {
		return err
	}
	raw, ok := cm.Data["result.json"]
	if !ok {
		return fmt.Errorf("result configmap %s missing result.json", key.Name)
	}
	return json.Unmarshal([]byte(raw), out)
}

// deleteResult removes a consumed result ConfigMap (best-effort).
func (r *KnowledgeBaseReconciler) deleteResult(ctx context.Context, ns, jobName string) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: resultConfigMapName(jobName)}}
	_ = r.Delete(ctx, cm)
}

// readCheckpointResult reads the checkpoint ConfigMap and returns completed
// sources, or nil if no checkpoint exists.
func (r *KnowledgeBaseReconciler) readCheckpointResult(ctx context.Context, ns, jobName string) []ragv1alpha1.IngestSourceResult {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: ns, Name: checkpointConfigMapName(jobName)}
	if err := r.Get(ctx, key, &cm); err != nil {
		return nil
	}
	raw, ok := cm.Data["checkpoint.json"]
	if !ok || raw == "" {
		return nil
	}
	var checkpoint struct {
		CompletedSources []ragv1alpha1.IngestSourceResult `json:"completedSources"`
	}
	if err := json.Unmarshal([]byte(raw), &checkpoint); err != nil {
		return nil
	}
	return checkpoint.CompletedSources
}

// deleteSpecConfigMap removes the worker spec ConfigMap (best-effort).
func (r *KnowledgeBaseReconciler) deleteSpecConfigMap(ctx context.Context, ns, jobName string) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: specConfigMapName(jobName)}}
	_ = r.Delete(ctx, cm)
}

// deleteCheckpointConfigMap removes a consumed checkpoint ConfigMap (best-effort).
func (r *KnowledgeBaseReconciler) deleteCheckpointConfigMap(ctx context.Context, ns, jobName string) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: checkpointConfigMapName(jobName)}}
	_ = r.Delete(ctx, cm)
}

// workerImage resolves the worker image for a KB.
func workerImage(kb *ragv1alpha1.KnowledgeBase) string {
	if kb.Spec.WorkerImage != "" {
		return kb.Spec.WorkerImage
	}
	return defaultWorkerImage
}

// secretEnv builds an EnvVar sourced from a Secret key.
func secretEnv(name string, ref *ragv1alpha1.SecretKeyRef) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
				Key:                  ref.Key,
			},
		},
	}
}

// credentialEnv gathers all secret-backed env vars implied by the spec.
func credentialEnv(kb *ragv1alpha1.KnowledgeBase) []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, s := range kb.Spec.Sources {
		switch {
		case s.GitHub != nil && s.GitHub.TokenSecretRef != nil:
			env = append(env, secretEnv("GITHUB_TOKEN_"+s.Name, s.GitHub.TokenSecretRef))
		case s.S3 != nil:
			if s.S3.AccessKeySecretRef != nil {
				env = append(env, secretEnv("S3_ACCESS_KEY_"+s.Name, s.S3.AccessKeySecretRef))
			}
			if s.S3.SecretKeySecretRef != nil {
				env = append(env, secretEnv("S3_SECRET_KEY_"+s.Name, s.S3.SecretKeySecretRef))
			}
		}
	}
	if kb.Spec.Embedding.APIKeySecretRef != nil {
		env = append(env, secretEnv("EMBEDDING_API_KEY", kb.Spec.Embedding.APIKeySecretRef))
	}
	if kb.Spec.VectorStore.CredentialsSecretRef != nil {
		env = append(env, secretEnv("VECTORSTORE_CREDENTIAL", kb.Spec.VectorStore.CredentialsSecretRef))
	}
	return env
}

// resourceRequirements maps the trimmed spec into a core/v1 ResourceRequirements.
func resourceRequirements(rr *ragv1alpha1.ResourceRequirements) (corev1.ResourceRequirements, error) {
	out := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	// Sensible defaults for an embedding workload (onnxruntime + batches are
	// memory-hungry; 2Gi OOM-kills on non-trivial corpora).
	out.Requests[corev1.ResourceCPU] = resource.MustParse("250m")
	out.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
	out.Limits[corev1.ResourceCPU] = resource.MustParse("2")
	out.Limits[corev1.ResourceMemory] = resource.MustParse("4Gi")
	if rr != nil {
		if rr.CPU != "" {
			q, err := resource.ParseQuantity(rr.CPU)
			if err != nil {
				return out, fmt.Errorf("invalid ingestion resources.cpu %q: %w", rr.CPU, err)
			}
			out.Requests[corev1.ResourceCPU] = q
			out.Limits[corev1.ResourceCPU] = q
		}
		if rr.Memory != "" {
			q, err := resource.ParseQuantity(rr.Memory)
			if err != nil {
				return out, fmt.Errorf("invalid ingestion resources.memory %q: %w", rr.Memory, err)
			}
			out.Requests[corev1.ResourceMemory] = q
			out.Limits[corev1.ResourceMemory] = q
		}
	}
	return out, nil
}

// specConfigMapName returns the ConfigMap name for the worker spec mount.
func specConfigMapName(jobName string) string { return nameWithSuffix(jobName, "-spec") }

// specConfigMapSizeOK returns true when the spec JSON fits within the
// Kubernetes ConfigMap size boundary (1 MiB max, with overhead headroom).
func specConfigMapSizeOK(specJSON string) bool {
	return len(specJSON) <= maxConfigMapBytes
}

// specConfigMap builds a ConfigMap holding the serialised spec for the worker.
func specConfigMap(ns, name, specJSON string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{labelManagedBy: "kuberag"},
		},
		Data: map[string]string{"spec.json": specJSON},
	}
}

// traceEnv injects the current OTel context as environment variables so the
// worker process can continue the trace.
func traceEnv(ctx context.Context) []corev1.EnvVar {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	env := make([]corev1.EnvVar, 0, len(carrier))
	for k, v := range carrier {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	return env
}

// baseJob assembles the common Job skeleton for a worker invocation.
// specCMName names the ConfigMap mounted at /etc/kuberag/spec.json.
func baseJob(ctx context.Context, kb *ragv1alpha1.KnowledgeBase, name, jobTypeLabel, hash, secretsHash, specCMName string, args []string, extraEnv []corev1.EnvVar) (*batchv1.Job, error) {
	backoff := int32(2)
	ttl := int32(300)
	if kb.Spec.Ingestion.TTLSecondsAfterFinished != nil {
		ttl = *kb.Spec.Ingestion.TTLSecondsAfterFinished
	}
	activeDeadline := int64(7200)
	if kb.Spec.Ingestion.ActiveDeadlineSeconds != nil {
		activeDeadline = *kb.Spec.Ingestion.ActiveDeadlineSeconds
	}
	sa := workerServiceAccountName(kb)

	env := []corev1.EnvVar{
		{Name: "KB_NAME", Value: kb.Name},
		{Name: "KB_NAMESPACE", Value: kb.Namespace},
		{Name: "KB_SPEC_PATH", Value: "/etc/kuberag/spec.json"},
		{Name: "RESULT_CONFIGMAP", Value: resultConfigMapName(name)},
	}
	env = append(env, traceEnv(ctx)...)
	env = append(env, scratchEnv()...)
	env = append(env, extraEnv...)
	env = append(env, credentialEnv(kb)...)

	resources, err := resourceRequirements(kb.Spec.Ingestion.Resources)
	if err != nil {
		return nil, err
	}

	labels := map[string]string{
		labelManagedBy:   "kuberag",
		labelKB:          kb.Name,
		labelJobType:     jobTypeLabel,
		labelSpecHash:    hash,
		labelSecretsHash: secretsHash,
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kb.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &activeDeadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					ServiceAccountName:            sa,
					PriorityClassName:             "kuberag-system",
					TerminationGracePeriodSeconds: ptr.To(int64(120)),
					SecurityContext:               hardenedPodSecurityContext(),
					Volumes:                       []corev1.Volume{scratchVolume(kb.Spec.Ingestion.ModelCacheSizeLimit), specVolume(specCMName)},
					NodeSelector:                  kb.Spec.Ingestion.NodeSelector,
					Tolerations:                   kb.Spec.Ingestion.Tolerations,
					Affinity:                      kb.Spec.Ingestion.Affinity,
					Containers: []corev1.Container{
						{
							Name:            "worker",
							Image:           workerImage(kb),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            args,
							Env:             env,
							Resources:       resources,
							SecurityContext: hardenedContainerSecurityContext(),
							VolumeMounts:    []corev1.VolumeMount{scratchMount(), specMount()},
						},
					},
				},
			},
		},
	}, nil
}

// buildIngestJob renders the ingestion Job (clone/chunk/embed/upsert).
func buildIngestJob(ctx context.Context, kb *ragv1alpha1.KnowledgeBase, hash, secretsHash string, mode ragv1alpha1.IngestMode, effChunking ragv1alpha1.ChunkingSpec) (*batchv1.Job, string, error) {
	specJSON, err := marshalEffectiveSpec(kb, effChunking)
	if err != nil {
		return nil, "", err
	}
	name := truncName(fmt.Sprintf("%s-ingest-r%d-t%d-c%s-%s",
		kb.Name, kb.Status.IngestRound, kb.Status.AutoTuneAttempts, chunkFingerprint(effChunking), hash))
	resumeFromCheckpoint := len(kb.Status.LastCheckpoint) > 0
	env := []corev1.EnvVar{
		{Name: "INGEST_MODE", Value: string(mode)},
		{Name: "INGEST_ROUND", Value: fmt.Sprintf("%d", kb.Status.IngestRound)},
		{Name: "PRIOR_SOURCES_JSON", Value: priorSourcesJSON(kb)},
		{Name: "CHECKPOINT_CONFIGMAP", Value: checkpointConfigMapName(name)},
	}
	if resumeFromCheckpoint {
		env = append(env, corev1.EnvVar{Name: "RESUME_FROM_CHECKPOINT", Value: "true"})
	}
	job, err := baseJob(ctx, kb, name, jobTypeIngest, hash, secretsHash, specConfigMapName(name), []string{"ingest"}, env)
	if err != nil {
		return nil, "", err
	}
	job.Labels[labelChunking] = chunkingLabel(effChunking)
	return job, specJSON, nil
}

// buildEvalJob renders the retrieval-quality evaluation Job. The round counter
// makes each evaluation a fresh Job (the spec hash is stable across evals).
func buildEvalJob(ctx context.Context, kb *ragv1alpha1.KnowledgeBase, hash, secretsHash string, round int, effChunking ragv1alpha1.ChunkingSpec) (*batchv1.Job, string, error) {
	specJSON, err := marshalQuerySpec(kb)
	if err != nil {
		return nil, "", err
	}
	rq := kb.Spec.RetrievalQuality
	name := truncName(fmt.Sprintf("%s-eval-r%d", kb.Name, round))
	env := []corev1.EnvVar{
		{Name: "EVAL_DATASET_CONFIGMAP", Value: rq.DatasetRef.Name},
		{Name: "EVAL_TOPK", Value: fmt.Sprintf("%d", defaultInt(rq.TopK, 8))},
	}
	job, err := baseJob(ctx, kb, name, jobTypeEval, hash, secretsHash, specConfigMapName(name), []string{"eval"}, env)
	if err != nil {
		return nil, "", err
	}
	job.Labels[labelChunking] = chunkingLabel(effChunking)
	return job, specJSON, nil
}

// buildCleanupJob renders the teardown Job that drops the remote collection.
func buildCleanupJob(ctx context.Context, kb *ragv1alpha1.KnowledgeBase, secretsHash string) (*batchv1.Job, string, error) {
	specJSON, err := marshalStoreSpec(kb)
	if err != nil {
		return nil, "", err
	}
	name := truncName(fmt.Sprintf("%s-cleanup", kb.Name))
	job, err := baseJob(ctx, kb, name, jobTypeCleanup, "", secretsHash, specConfigMapName(name), []string{"cleanup"}, nil)
	if err != nil {
		return nil, "", err
	}
	zero := int32(1)
	job.Spec.BackoffLimit = &zero
	return job, specJSON, nil
}

// marshalEffectiveSpec serializes the spec with the effective chunking applied
// and the resolved embedding dimension injected so the worker never needs to
// probe the embedding API to discover it.
func marshalEffectiveSpec(kb *ragv1alpha1.KnowledgeBase, effChunking ragv1alpha1.ChunkingSpec) (string, error) {
	spec := kb.Spec.DeepCopy()
	spec.Chunking = effChunking
	// Pre-resolve the embedding dimension so the worker can skip the API probe.
	if spec.Embedding.Dimension == 0 {
		spec.Embedding.Dimension = embeddingDimension(kb.Spec.Embedding)
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// marshalQuerySpec emits only the fields needed to embed a query and connect
// to the vector store. This keeps eval/restore ConfigMaps independent of large
// source lists and include globs.
func marshalQuerySpec(kb *ragv1alpha1.KnowledgeBase) (string, error) {
	embedding := kb.Spec.Embedding.DeepCopy()
	if embedding.Dimension == 0 {
		embedding.Dimension = embeddingDimension(kb.Spec.Embedding)
	}
	spec := struct {
		Embedding   ragv1alpha1.EmbeddingSpec   `json:"embedding"`
		VectorStore ragv1alpha1.VectorStoreSpec `json:"vectorStore"`
	}{
		Embedding:   *embedding,
		VectorStore: kb.Spec.VectorStore,
	}
	b, err := json.Marshal(spec)
	return string(b), err
}

// marshalStoreSpec emits the minimum cleanup/backup configuration.
func marshalStoreSpec(kb *ragv1alpha1.KnowledgeBase) (string, error) {
	spec := struct {
		VectorStore ragv1alpha1.VectorStoreSpec `json:"vectorStore"`
	}{
		VectorStore: kb.Spec.VectorStore,
	}
	b, err := json.Marshal(spec)
	return string(b), err
}

// priorSourcesJSON serializes last-synced revisions so the worker can do incremental
// sync. It also merges in LastCheckpoint so a resume after failure skips already-
// completed sources.
func priorSourcesJSON(kb *ragv1alpha1.KnowledgeBase) string {
	sources := kb.Status.Sources
	if len(kb.Status.LastCheckpoint) > 0 {
		seen := map[string]bool{}
		for _, s := range sources {
			seen[s.Name] = true
		}
		for _, c := range kb.Status.LastCheckpoint {
			if !seen[c.Name] {
				sources = append(sources, ragv1alpha1.SourceStatus(c))
			}
		}
	}
	b, _ := json.Marshal(sources)
	return string(b)
}

func truncName(name string) string {
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	hash := fmt.Sprintf("%x", sum[:4])
	prefix := strings.TrimRight(name[:63-1-len(hash)], "-")
	return prefix + "-" + hash
}

func nameWithSuffix(name, suffix string) string {
	if len(name)+len(suffix) <= 63 {
		return name + suffix
	}
	sum := sha256.Sum256([]byte(name + suffix))
	hash := fmt.Sprintf("%x", sum[:4])
	maxBase := 63 - len(suffix) - 1 - len(hash)
	base := strings.TrimRight(name[:maxBase], "-")
	return base + "-" + hash + suffix
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// ignoreAlreadyExists swallows AlreadyExists so Job creation is idempotent.
func ignoreAlreadyExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// nonRootUID is distroless/nonroot's uid:gid; the worker/retriever run as it.
const nonRootUID int64 = 65532

// hardenedPodSecurityContext enforces non-root, fsGroup, and the default seccomp profile.
func hardenedPodSecurityContext() *corev1.PodSecurityContext {
	uid := nonRootUID
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		RunAsUser:      &uid,
		RunAsGroup:     &uid,
		FSGroup:        &uid,
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// hardenedContainerSecurityContext drops all capabilities, forbids privilege
// escalation, enforces non-root, sets the default seccomp profile, and runs
// with a read-only root filesystem (scratch is mounted).
func hardenedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// scratchVolume / scratchMount give the read-only-rootfs containers a writable
// place for clones, temp files, and model caches.
func scratchVolume(cacheSizeLimit string) corev1.Volume {
	vol := corev1.Volume{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	if cacheSizeLimit != "" {
		sz := resource.MustParse(cacheSizeLimit)
		vol.EmptyDir.SizeLimit = &sz
	}
	return vol
}

func scratchMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: "scratch", MountPath: "/scratch"}
}

// scratchEnv points HOME, caches, and temp dirs at the writable scratch mount.
func scratchEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "HOME", Value: "/scratch"},
		{Name: "TMPDIR", Value: "/scratch"},
		{Name: "XDG_CACHE_HOME", Value: "/scratch/.cache"},
		{Name: "HF_HOME", Value: "/scratch/.cache/huggingface"},
		{Name: "FASTEMBED_CACHE_PATH", Value: "/scratch/.cache/fastembed"},
		{Name: "PYTHONDONTWRITEBYTECODE", Value: "1"},
	}
}

// specVolume mounts a ConfigMap holding the KnowledgeBase spec JSON.
func specVolume(cmName string) corev1.Volume {
	return corev1.Volume{
		Name: "spec",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
			},
		},
	}
}

// specMount is the mount point for the spec ConfigMap.
func specMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: "spec", MountPath: "/etc/kuberag", ReadOnly: true}
}
