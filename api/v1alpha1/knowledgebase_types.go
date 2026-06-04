package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// SourceType enumerates the supported knowledge sources.
// +kubebuilder:validation:Enum=github;s3;web
type SourceType string

const (
	SourceGitHub SourceType = "github"
	SourceS3     SourceType = "s3"
	SourceWeb    SourceType = "web"
)

// Source describes one place to pull documents from.
// +kubebuilder:validation:XValidation:rule="self.type != 'github' || has(self.github)",message="github block required when type is github"
// +kubebuilder:validation:XValidation:rule="self.type != 's3' || has(self.s3)",message="s3 block required when type is s3"
// +kubebuilder:validation:XValidation:rule="self.type != 'web' || has(self.web)",message="web block required when type is web"
type Source struct {
	// Name uniquely identifies this source within the KnowledgeBase. Used to
	// track per-source sync state for incremental ingestion.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Type of the source.
	Type SourceType `json:"type"`

	// +optional
	GitHub *GitHubSource `json:"github,omitempty"`
	// +optional
	S3 *S3Source `json:"s3,omitempty"`
	// +optional
	Web *WebSource `json:"web,omitempty"`
}

// GitHubSource points at a GitHub repository.
type GitHubSource struct {
	// Repo in "owner/name" form, e.g. "qdrant/qdrant".
	Repo string `json:"repo"`
	// Ref is the branch, tag or commit to ingest. Defaults to the repo default branch.
	// +optional
	Ref string `json:"ref,omitempty"`
	// IncludeGlobs limits ingestion to matching paths (e.g. "docs/**", "**/*.md").
	// +optional
	IncludeGlobs []string `json:"includeGlobs,omitempty"`
	// TokenSecretRef references a Secret key holding a GitHub token for private repos.
	// +optional
	TokenSecretRef *SecretKeyRef `json:"tokenSecretRef,omitempty"`
}

// S3Source points at objects in an S3-compatible bucket.
type S3Source struct {
	Bucket string `json:"bucket"`
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// +optional
	Region string `json:"region,omitempty"`
	// Endpoint for S3-compatible stores (e.g. MinIO). Empty means AWS S3.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	IncludeGlobs []string `json:"includeGlobs,omitempty"`
	// AccessKeySecretRef and SecretKeySecretRef hold credentials. If unset, the
	// worker falls back to the ambient credential chain (IRSA, instance role).
	// +optional
	AccessKeySecretRef *SecretKeyRef `json:"accessKeySecretRef,omitempty"`
	// +optional
	SecretKeySecretRef *SecretKeyRef `json:"secretKeySecretRef,omitempty"`
}

// WebSource crawls one or more seed URLs.
type WebSource struct {
	// +kubebuilder:validation:MinItems=1
	URLs []string `json:"urls"`
	// MaxDepth bounds link-following from each seed. 0 means only the seed pages.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxDepth int `json:"maxDepth,omitempty"`
	// SameDomainOnly restricts crawling to the seed's domain.
	// +kubebuilder:default=true
	// +optional
	SameDomainOnly *bool `json:"sameDomainOnly,omitempty"`
	// MaxPages caps the total number of fetched pages.
	// +kubebuilder:default=200
	// +optional
	MaxPages int `json:"maxPages,omitempty"`
}

// ---------------------------------------------------------------------------
// Chunking & embedding
// ---------------------------------------------------------------------------

// ChunkingStrategy enumerates how documents get split.
// +kubebuilder:validation:Enum=semantic;recursive;fixed
type ChunkingStrategy string

const (
	ChunkSemantic  ChunkingStrategy = "semantic"
	ChunkRecursive ChunkingStrategy = "recursive"
	ChunkFixed     ChunkingStrategy = "fixed"
)

// ChunkingSpec controls how source documents are split into chunks.
// +kubebuilder:validation:XValidation:rule="self.overlap < self.maxTokens",message="overlap must be strictly less than maxTokens"
type ChunkingSpec struct {
	// +kubebuilder:default=semantic
	// +optional
	Strategy ChunkingStrategy `json:"strategy,omitempty"`
	// +kubebuilder:default=800
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxTokens int `json:"maxTokens,omitempty"`
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=0
	// +optional
	Overlap int `json:"overlap,omitempty"`
}

// EmbeddingSpec selects the embedding model. Changing Model triggers a full re-embed.
// +kubebuilder:validation:XValidation:rule="self.provider != 'openai-compatible' || (has(self.baseURL) && size(self.baseURL) > 0)",message="baseURL is required when provider is openai-compatible"
type EmbeddingSpec struct {
	// Model name, e.g. "bge-small", "bge-large", "text-embedding-3-small".
	// +kubebuilder:default=bge-small
	Model string `json:"model"`
	// Provider of the model:
	//   local            - run the model in-process via fastembed
	//   openai           - OpenAI embeddings API
	//   gemini           - Google Gemini embeddings (OpenAI-compatible endpoint)
	//   openai-compatible- any OpenAI-compatible /embeddings endpoint (set baseURL)
	// +kubebuilder:validation:Enum=local;openai;gemini;openai-compatible
	// +kubebuilder:default=local
	// +optional
	Provider string `json:"provider,omitempty"`
	// BaseURL overrides the API base URL. Required for "openai-compatible";
	// optional override for the other hosted providers.
	// +optional
	BaseURL string `json:"baseURL,omitempty"`
	// Dimension overrides the vector dimension. If unset it is taken from a
	// built-in table for known models, otherwise auto-detected at ingest time.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Dimension int `json:"dimension,omitempty"`
	// APIKeySecretRef holds the provider API key when Provider != local.
	// +optional
	APIKeySecretRef *SecretKeyRef `json:"apiKeySecretRef,omitempty"`
}

// ---------------------------------------------------------------------------
// Vector store
// ---------------------------------------------------------------------------

// VectorStoreType enumerates supported vector databases.
// +kubebuilder:validation:Enum=qdrant;pgvector;milvus
type VectorStoreType string

const (
	VectorStoreQdrant   VectorStoreType = "qdrant"
	VectorStorePgVector VectorStoreType = "pgvector"
	VectorStoreMilvus   VectorStoreType = "milvus"
)

// DistanceMetric enumerates supported vector distance metrics.
// +kubebuilder:validation:Enum=cosine;dot;euclid
type DistanceMetric string

const (
	DistanceCosine DistanceMetric = "cosine"
	DistanceDot    DistanceMetric = "dot"
	DistanceEuclid DistanceMetric = "euclid"
)

// VectorStoreSpec describes where embeddings are written.
type VectorStoreSpec struct {
	// +kubebuilder:default=qdrant
	Type VectorStoreType `json:"type"`
	// Endpoint of the vector store, e.g. "http://qdrant:6333" or a
	// "postgresql://host/db" DSN for pgvector.
	Endpoint string `json:"endpoint"`
	// Collection (or table) name. Defaults to the KnowledgeBase name.
	// +optional
	Collection string `json:"collection,omitempty"`
	// +kubebuilder:default=cosine
	// +optional
	Distance DistanceMetric `json:"distance,omitempty"`
	// CredentialsSecretRef holds a password/API key for the store when needed.
	// +optional
	CredentialsSecretRef *SecretKeyRef `json:"credentialsSecretRef,omitempty"`
}

// ---------------------------------------------------------------------------
// Freshness, ingestion mode, retrieval quality
// ---------------------------------------------------------------------------

// FreshnessSpec controls how often the source is re-synced.
type FreshnessSpec struct {
	// Schedule is a standard cron expression (5 fields). Empty disables scheduled reindexing.
	// +optional
	Schedule string `json:"schedule,omitempty"`
}

// IngestMode selects full vs incremental ingestion.
// +kubebuilder:validation:Enum=full;incremental
type IngestMode string

const (
	IngestFull        IngestMode = "full"
	IngestIncremental IngestMode = "incremental"
)

// IngestionSpec tunes how ingestion Jobs run.
type IngestionSpec struct {
	// Mode "incremental" only re-embeds changed/added chunks and deletes removed ones.
	// "full" recreates the collection every run.
	// +kubebuilder:default=incremental
	// +optional
	Mode IngestMode `json:"mode,omitempty"`
	// Resources for the ingestion worker pod.
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`
	// ServiceAccountName runs the ingestion Job under a specific SA (e.g. for IRSA).
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// NodeSelector is a selector which must be true for the pod to fit on a node.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations allows the pod to tolerate node taints.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Affinity controls pod scheduling preferences.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

// ResourceRequirements is a trimmed mirror of core/v1 requests+limits.
type ResourceRequirements struct {
	// +optional
	CPU string `json:"cpu,omitempty"`
	// +optional
	Memory string `json:"memory,omitempty"`
}

// RetrievalQualitySpec enables periodic evaluation and optional auto-tuning.
type RetrievalQualitySpec struct {
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// EvalSchedule is a cron expression for running evaluations.
	// +optional
	EvalSchedule string `json:"evalSchedule,omitempty"`
	// DatasetRef references a ConfigMap with key "dataset.jsonl"; each line is
	// {"query": "...", "expectedSources": ["path/a", "path/b"]}.
	DatasetRef LocalObjectRef `json:"datasetRef"`
	// TopK used during evaluation retrieval.
	// +kubebuilder:default=8
	// +optional
	TopK int `json:"topK,omitempty"`
	// MinimumRecallPercent is the recall@TopK target (0-100). Below this the KB is
	// marked degraded and, if AutoTune is enabled, the operator adjusts chunking.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	MinimumRecallPercent int `json:"minimumRecallPercent,omitempty"`
	// +optional
	AutoTune *AutoTuneSpec `json:"autoTune,omitempty"`
}

// AutoTuneSpec controls automatic chunking adjustments to hit a recall target.
type AutoTuneSpec struct {
	// +kubebuilder:default=false
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// MaxAttempts bounds how many tuning iterations the operator will try.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxAttempts int `json:"maxAttempts,omitempty"`
}

// ---------------------------------------------------------------------------
// Spec / Status
// ---------------------------------------------------------------------------

// KnowledgeBaseSpec is the desired knowledge state.
// +kubebuilder:validation:XValidation:rule="self.sources.all(s1, size(self.sources.filter(s2, s2.name == s1.name)) == 1)",message="source names must be unique within a KnowledgeBase"
type KnowledgeBaseSpec struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=5
	Sources []Source `json:"sources"`
	// +optional
	Chunking ChunkingSpec `json:"chunking,omitempty"`

	Embedding EmbeddingSpec `json:"embedding"`

	VectorStore VectorStoreSpec `json:"vectorStore"`
	// +optional
	Freshness FreshnessSpec `json:"freshness,omitempty"`
	// +optional
	Ingestion IngestionSpec `json:"ingestion,omitempty"`
	// +optional
	RetrievalQuality *RetrievalQualitySpec `json:"retrievalQuality,omitempty"`
	// WorkerImage overrides the ingestion/eval worker container image.
	// +optional
	WorkerImage string `json:"workerImage,omitempty"`
	// Suspend pauses all reconciliation actions (no new Jobs) when true.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// KnowledgeBasePhase is a coarse, human-readable lifecycle state.
// +kubebuilder:validation:Enum=Pending;Ingesting;Ready;Degraded;Failed;Suspended
type KnowledgeBasePhase string

const (
	PhasePending   KnowledgeBasePhase = "Pending"
	PhaseIngesting KnowledgeBasePhase = "Ingesting"
	PhaseReady     KnowledgeBasePhase = "Ready"
	PhaseDegraded  KnowledgeBasePhase = "Degraded"
	PhaseFailed    KnowledgeBasePhase = "Failed"
	PhaseSuspended KnowledgeBasePhase = "Suspended"
)

// SourceStatus tracks the last-synced fingerprint per source for incremental sync.
type SourceStatus struct {
	Name string `json:"name"`
	// Revision is the source-specific marker last ingested (git SHA, S3 ETag set hash, crawl hash).
	// +optional
	Revision string `json:"revision,omitempty"`
	// +optional
	Chunks int `json:"chunks,omitempty"`
}

// EvaluationStatus records the most recent retrieval-quality run.
type EvaluationStatus struct {
	// +optional
	RecallPercent int `json:"recallPercent,omitempty"`
	// +optional
	P95LatencyMillis int `json:"p95LatencyMillis,omitempty"`
	// +optional
	Queries int `json:"queries,omitempty"`
	// +optional
	Time *metav1.Time `json:"time,omitempty"`
}

// KnowledgeBaseStatus is the observed knowledge state.
type KnowledgeBaseStatus struct {
	// +optional
	Phase KnowledgeBasePhase `json:"phase,omitempty"`
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ObservedSpecHash fingerprints the re-ingest-relevant spec fields.
	// +optional
	ObservedSpecHash string `json:"observedSpecHash,omitempty"`
	// ObservedEmbeddingModel is the model used for the data currently in the store.
	// +optional
	ObservedEmbeddingModel string `json:"observedEmbeddingModel,omitempty"`
	// EffectiveChunking is the chunking actually in use (spec, possibly auto-tuned).
	// +optional
	EffectiveChunking *ChunkingSpec `json:"effectiveChunking,omitempty"`
	// AutoTuneAttempts counts auto-tune iterations applied so far.
	// +optional
	AutoTuneAttempts int `json:"autoTuneAttempts,omitempty"`
	// PendingRetune marks that auto-tune has changed the effective chunking and a
	// re-index is owed, without disturbing ObservedSpecHash (which keeps tracking
	// the last ingested spec so user spec edits are still detected mid-tune).
	// +optional
	PendingRetune bool `json:"pendingRetune,omitempty"`
	// BestRecallPercent is the highest recall observed across auto-tune attempts.
	// +optional
	BestRecallPercent int `json:"bestRecallPercent,omitempty"`
	// BestChunking is the chunking that achieved BestRecallPercent. When auto-tune
	// exhausts its attempts without meeting the target, the operator lands the KB
	// on this configuration rather than the last (arbitrary) ladder step.
	// +optional
	BestChunking *ChunkingSpec `json:"bestChunking,omitempty"`
	// EvalRound increments per evaluation run so each eval Job gets a unique name
	// (the spec hash alone is stable across repeated evaluations).
	// +optional
	EvalRound int `json:"evalRound,omitempty"`
	// +optional
	LastIndexedTime *metav1.Time `json:"lastIndexedTime,omitempty"`
	// IndexedChunks is the total number of chunks in the store after the last run.
	// +optional
	IndexedChunks int `json:"indexedChunks,omitempty"`
	// +optional
	Sources []SourceStatus `json:"sources,omitempty"`
	// +optional
	Evaluation *EvaluationStatus `json:"evaluation,omitempty"`
	// ActiveJob is the name of the in-flight ingestion/eval Job, if any.
	// +optional
	ActiveJob string `json:"activeJob,omitempty"`
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kb
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.status.observedEmbeddingModel`
// +kubebuilder:printcolumn:name="Chunks",type=integer,JSONPath=`.status.indexedChunks`
// +kubebuilder:printcolumn:name="Recall",type=integer,JSONPath=`.status.evaluation.recallPercent`
// +kubebuilder:printcolumn:name="LastIndexed",type=date,JSONPath=`.status.lastIndexedTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KnowledgeBase is a declarative, reconciled RAG knowledge source.
type KnowledgeBase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KnowledgeBaseSpec   `json:"spec,omitempty"`
	Status KnowledgeBaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KnowledgeBaseList contains a list of KnowledgeBase.
type KnowledgeBaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBase `json:"items"`
}
