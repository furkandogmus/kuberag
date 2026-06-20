package controller

import (
	"context"
	"encoding/json"
	"fmt"

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
	defaultWorkerSA       = "kuberag-worker"

	labelManagedBy = "app.kubernetes.io/managed-by"
	labelKB        = "rag.furkan.dev/knowledgebase"
	labelJobType   = "rag.furkan.dev/job-type"
	labelSpecHash  = "rag.furkan.dev/spec-hash"

	jobTypeIngest  = "ingest"
	jobTypeEval    = "eval"
	jobTypeCleanup = "cleanup"
)

// jobType reads the job-type label from a Job.
func jobType(j *batchv1.Job) string { return j.Labels[labelJobType] }

// resultConfigMapName is where the worker writes its structured result.
func resultConfigMapName(jobName string) string { return jobName + "-result" }

// IngestResult is the JSON the ingestion worker writes to its result ConfigMap.
type IngestResult struct {
	TotalChunks int                  `json:"totalChunks"`
	Sources     []IngestSourceResult `json:"sources"`
}

// IngestSourceResult records per-source sync output for incremental tracking.
type IngestSourceResult struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
	Chunks   int    `json:"chunks"`
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

// baseJob assembles the common Job skeleton for a worker invocation.
func baseJob(kb *ragv1alpha1.KnowledgeBase, name, jobTypeLabel, hash string, args []string, extraEnv []corev1.EnvVar) (*batchv1.Job, error) {
	backoff := int32(2)
	// Short TTL so finished Jobs are GC'd well before the next scheduled run,
	// avoiding name collisions on freshness re-syncs of an unchanged spec.
	ttl := int32(300)
	sa := kb.Spec.Ingestion.ServiceAccountName
	if sa == "" {
		sa = defaultWorkerSA
	}

	env := []corev1.EnvVar{
		{Name: "KB_NAME", Value: kb.Name},
		{Name: "KB_NAMESPACE", Value: kb.Namespace},
		{Name: "RESULT_CONFIGMAP", Value: resultConfigMapName(name)},
	}
	env = append(env, scratchEnv()...)
	env = append(env, extraEnv...)
	env = append(env, credentialEnv(kb)...)

	resources, err := resourceRequirements(kb.Spec.Ingestion.Resources)
	if err != nil {
		return nil, err
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kb.Namespace,
			Labels: map[string]string{
				labelManagedBy: "kuberag",
				labelKB:        kb.Name,
				labelJobType:   jobTypeLabel,
				labelSpecHash:  hash,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					ServiceAccountName:            sa,
					PriorityClassName:             "kuberag-system",
					TerminationGracePeriodSeconds: ptr.To(int64(120)),
					SecurityContext:               hardenedPodSecurityContext(),
					Volumes:            []corev1.Volume{scratchVolume()},
					NodeSelector:       kb.Spec.Ingestion.NodeSelector,
					Tolerations:        kb.Spec.Ingestion.Tolerations,
					Affinity:           kb.Spec.Ingestion.Affinity,
					Containers: []corev1.Container{
						{
							Name:            "worker",
							Image:           workerImage(kb),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            args,
							Env:             env,
							Resources:       resources,
							SecurityContext: hardenedContainerSecurityContext(),
							VolumeMounts:    []corev1.VolumeMount{scratchMount()},
						},
					},
				},
			},
		},
	}, nil
}

// buildIngestJob renders the ingestion Job (clone/chunk/embed/upsert).
func buildIngestJob(kb *ragv1alpha1.KnowledgeBase, hash string, mode ragv1alpha1.IngestMode, effChunking ragv1alpha1.ChunkingSpec) (*batchv1.Job, error) {
	specJSON, err := marshalEffectiveSpec(kb, effChunking)
	if err != nil {
		return nil, err
	}
	// The spec hash is stable across auto-tune (which tunes *effective* chunking
	// and forces re-ingest via PendingRetune, not by changing the spec hash).
	// The ingestion round disambiguates scheduled runs and retries while the
	// tune attempt + chunk fingerprint identify auto-tune configurations.
	name := truncName(fmt.Sprintf("%s-ingest-r%d-t%d-c%s-%s",
		kb.Name, kb.Status.IngestRound, kb.Status.AutoTuneAttempts, chunkFingerprint(effChunking), hash))
	env := []corev1.EnvVar{
		{Name: "KB_SPEC_JSON", Value: specJSON},
		{Name: "INGEST_MODE", Value: string(mode)},
		{Name: "PRIOR_SOURCES_JSON", Value: priorSourcesJSON(kb)},
	}
	return baseJob(kb, name, jobTypeIngest, hash, []string{"ingest"}, env)
}

// buildEvalJob renders the retrieval-quality evaluation Job. The round counter
// makes each evaluation a fresh Job (the spec hash is stable across evals).
func buildEvalJob(kb *ragv1alpha1.KnowledgeBase, hash string, round int, effChunking ragv1alpha1.ChunkingSpec) (*batchv1.Job, error) {
	specJSON, err := marshalEffectiveSpec(kb, effChunking)
	if err != nil {
		return nil, err
	}
	rq := kb.Spec.RetrievalQuality
	name := truncName(fmt.Sprintf("%s-eval-r%d", kb.Name, round))
	env := []corev1.EnvVar{
		{Name: "KB_SPEC_JSON", Value: specJSON},
		{Name: "EVAL_DATASET_CONFIGMAP", Value: rq.DatasetRef.Name},
		{Name: "EVAL_TOPK", Value: fmt.Sprintf("%d", defaultInt(rq.TopK, 8))},
	}
	return baseJob(kb, name, jobTypeEval, hash, []string{"eval"}, env)
}

// buildCleanupJob renders the teardown Job that drops the remote collection.
func buildCleanupJob(kb *ragv1alpha1.KnowledgeBase) (*batchv1.Job, error) {
	specJSON, err := marshalEffectiveSpec(kb, effectiveChunking(kb))
	if err != nil {
		return nil, err
	}
	name := truncName(fmt.Sprintf("%s-cleanup", kb.Name))
	env := []corev1.EnvVar{{Name: "KB_SPEC_JSON", Value: specJSON}}
	job, err := baseJob(kb, name, jobTypeCleanup, "", []string{"cleanup"}, env)
	if err != nil {
		return nil, err
	}
	// Cleanup must not retry forever during deletion.
	zero := int32(1)
	job.Spec.BackoffLimit = &zero
	return job, nil
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

// priorSourcesJSON serializes last-synced revisions so the worker can do incremental sync.
func priorSourcesJSON(kb *ragv1alpha1.KnowledgeBase) string {
	b, _ := json.Marshal(kb.Status.Sources)
	return string(b)
}

func truncName(name string) string {
	if len(name) > 63 {
		return name[:63]
	}
	return name
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
// escalation, and runs with a read-only root filesystem (scratch is mounted).
func hardenedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// scratchVolume / scratchMount give the read-only-rootfs containers a writable
// place for clones, temp files, and model caches.
func scratchVolume() corev1.Volume {
	return corev1.Volume{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
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
