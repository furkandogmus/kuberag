package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RerankSpec configures optional cross-encoder reranking of retrieved chunks.
type RerankSpec struct {
	// +kubebuilder:default=false
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Model is the reranker model name (e.g. "bge-reranker-base").
	// +kubebuilder:default=bge-reranker-base
	// +optional
	Model string `json:"model,omitempty"`
	// CandidatePoolSize is how many candidates to retrieve before reranking;
	// the reranker then returns the top `topK`. A larger pool gives the reranker
	// more to work with (better quality) at the cost of latency. 0 means auto
	// (max(4×topK, 20)).
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	CandidatePoolSize int `json:"candidatePoolSize,omitempty"`
}

// GenerationSpec turns the retriever into a full RAG endpoint: after retrieval
// it asks an LLM to synthesize an answer grounded in the retrieved chunks.
// +kubebuilder:validation:XValidation:rule="self.provider != 'openai-compatible' || (has(self.baseURL) && size(self.baseURL) > 0)",message="baseURL is required when provider is openai-compatible"
type GenerationSpec struct {
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Provider of the chat model (all spoken via the OpenAI-compatible API):
	//   openai | openrouter | groq | gemini | openai-compatible
	// +kubebuilder:validation:Enum=openai;openrouter;groq;gemini;openai-compatible
	Provider string `json:"provider"`
	// Model name, e.g. "gpt-4o-mini", "google/gemini-2.0-flash-exp:free",
	// "llama-3.3-70b-versatile", "gemini-2.0-flash".
	Model string `json:"model"`
	// BaseURL overrides the provider's API base URL (required for openai-compatible).
	// +optional
	BaseURL string `json:"baseURL,omitempty"`
	// APIKeySecretRef holds the chat provider API key. Optional for local
	// OpenAI-compatible servers (Ollama, vLLM, LM Studio) that need no auth.
	// +optional
	APIKeySecretRef *SecretKeyRef `json:"apiKeySecretRef,omitempty"`
	// MaxTokens caps the generated answer length.
	// +kubebuilder:default=512
	// +optional
	MaxTokens int `json:"maxTokens,omitempty"`
	// SystemPrompt overrides the default grounding instruction.
	// +optional
	SystemPrompt string `json:"systemPrompt,omitempty"`
}

// RetrieverSpec declares a serving endpoint over a KnowledgeBase.
type RetrieverSpec struct {
	// KnowledgeBaseRef names the KnowledgeBase to serve from (same namespace).
	KnowledgeBaseRef LocalObjectRef `json:"knowledgeBaseRef"`
	// Generation, when set, enables LLM answer synthesis over retrieved chunks.
	// +optional
	Generation *GenerationSpec `json:"generation,omitempty"`
	// TopK is the default number of chunks returned per query.
	// +kubebuilder:default=8
	// +kubebuilder:validation:Minimum=1
	// +optional
	TopK int `json:"topK,omitempty"`
	// Hybrid enables hybrid retrieval (dense vector + lexical search fused with
	// Reciprocal Rank Fusion) by default for every query. Individual requests can
	// still override it via the `hybrid` field on /query. Best when exact
	// keywords/identifiers matter as much as semantic similarity.
	// +kubebuilder:default=false
	// +optional
	Hybrid bool `json:"hybrid,omitempty"`
	// HybridDensePercent weights dense (vector) vs lexical search when fusing
	// hybrid results with RRF: the dense contribution is scaled by this percent
	// and the lexical by the remainder (e.g. 70 => 0.7 dense / 0.3 lexical).
	// Only applies when hybrid retrieval is active. Unset = 50 (equal weighting);
	// 0 = pure lexical, 100 = pure dense.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	HybridDensePercent *int `json:"hybridDensePercent,omitempty"`
	// ScoreThresholdPercent drops results below this similarity (0-100).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	ScoreThresholdPercent int `json:"scoreThresholdPercent,omitempty"`
	// +optional
	Rerank *RerankSpec `json:"rerank,omitempty"`
	// Replicas of the retriever server.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// Image overrides the retriever server image.
	// +optional
	Image string `json:"image,omitempty"`
	// Resources defines resource requirements for the retriever pod.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
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

// RetrieverStatus is the observed serving state.
type RetrieverStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ServiceEndpoint is the in-cluster URL of the retriever, when available.
	// +optional
	ServiceEndpoint string `json:"serviceEndpoint,omitempty"`
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.readyReplicas
// +kubebuilder:resource:shortName=rtr
// +kubebuilder:printcolumn:name="KnowledgeBase",type=string,JSONPath=`.spec.knowledgeBaseRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.serviceEndpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Retriever serves low-latency retrieval over a KnowledgeBase.
type Retriever struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RetrieverSpec   `json:"spec,omitempty"`
	Status RetrieverStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RetrieverList contains a list of Retriever.
type RetrieverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Retriever `json:"items"`
}
