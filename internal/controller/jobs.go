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

	ragv1alpha1 "github.com/furkandogmus/rag-operator/api/v1alpha1"
)

const (
	defaultWorkerImage    = "ghcr.io/furkandogmus/rag-worker:latest"
	defaultRetrieverImage = "ghcr.io/furkandogmus/rag-retriever:latest"
	defaultWorkerSA       = "rag-operator-worker"

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
func resourceRequirements(rr *ragv1alpha1.ResourceRequirements) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	// Sensible defaults for an embedding workload (onnxruntime + batches are
	// memory-hungry; 2Gi OOM-kills on non-trivial corpora).
	out.Requests[corev1.ResourceCPU] = resource.MustParse("250m")
	out.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
	out.Limits[corev1.ResourceMemory] = resource.MustParse("4Gi")
	if rr != nil {
		if rr.CPU != "" {
			q := resource.MustParse(rr.CPU)
			out.Requests[corev1.ResourceCPU] = q
			out.Limits[corev1.ResourceCPU] = q
		}
		if rr.Memory != "" {
			q := resource.MustParse(rr.Memory)
			out.Requests[corev1.ResourceMemory] = q
			out.Limits[corev1.ResourceMemory] = q
		}
	}
	return out
}

// baseJob assembles the common Job skeleton for a worker invocation.
func baseJob(kb *ragv1alpha1.KnowledgeBase, name, jobTypeLabel, hash string, args []string, extraEnv []corev1.EnvVar) *batchv1.Job {
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
	env = append(env, extraEnv...)
	env = append(env, credentialEnv(kb)...)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kb.Namespace,
			Labels: map[string]string{
				labelManagedBy: "rag-operator",
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
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: sa,
					Containers: []corev1.Container{
						{
							Name:            "worker",
							Image:           workerImage(kb),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            args,
							Env:             env,
							Resources:       resourceRequirements(kb.Spec.Ingestion.Resources),
						},
					},
				},
			},
		},
	}
}

// buildIngestJob renders the ingestion Job (clone/chunk/embed/upsert).
func buildIngestJob(kb *ragv1alpha1.KnowledgeBase, hash string, mode ragv1alpha1.IngestMode, effChunking ragv1alpha1.ChunkingSpec) (*batchv1.Job, error) {
	specJSON, err := marshalEffectiveSpec(kb, effChunking)
	if err != nil {
		return nil, err
	}
	name := truncName(fmt.Sprintf("%s-ingest-%s", kb.Name, hash))
	env := []corev1.EnvVar{
		{Name: "KB_SPEC_JSON", Value: specJSON},
		{Name: "INGEST_MODE", Value: string(mode)},
		{Name: "PRIOR_SOURCES_JSON", Value: priorSourcesJSON(kb)},
	}
	return baseJob(kb, name, jobTypeIngest, hash, []string{"ingest"}, env), nil
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
	return baseJob(kb, name, jobTypeEval, hash, []string{"eval"}, env), nil
}

// buildCleanupJob renders the teardown Job that drops the remote collection.
func buildCleanupJob(kb *ragv1alpha1.KnowledgeBase) (*batchv1.Job, error) {
	specJSON, err := marshalEffectiveSpec(kb, effectiveChunking(kb))
	if err != nil {
		return nil, err
	}
	name := truncName(fmt.Sprintf("%s-cleanup", kb.Name))
	env := []corev1.EnvVar{{Name: "KB_SPEC_JSON", Value: specJSON}}
	job := baseJob(kb, name, jobTypeCleanup, "", []string{"cleanup"}, env)
	// Cleanup must not retry forever during deletion.
	zero := int32(1)
	job.Spec.BackoffLimit = &zero
	return job, nil
}

// marshalEffectiveSpec serializes the spec with the effective chunking applied.
func marshalEffectiveSpec(kb *ragv1alpha1.KnowledgeBase, effChunking ragv1alpha1.ChunkingSpec) (string, error) {
	spec := kb.Spec.DeepCopy()
	spec.Chunking = effChunking
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
