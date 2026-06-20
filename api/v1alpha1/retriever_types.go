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

// RateLimitSpec configures a per-client token-bucket limiter for the Retriever.
type RateLimitSpec struct {
	// Enabled turns rate limiting on. When disabled, RequestsPerMinute and Burst
	// are ignored.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// RequestsPerMinute is the sustained request rate allowed per client IP.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100000
	// +optional
	RequestsPerMinute int `json:"requestsPerMinute,omitempty"`
	// Burst is the maximum number of immediately available request tokens.
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	// +optional
	Burst int `json:"burst,omitempty"`
}

// AutoscalingSpec configures a CPU-based HorizontalPodAutoscaler.
// +kubebuilder:validation:XValidation:rule="self.maxReplicas >= self.minReplicas",message="maxReplicas must be greater than or equal to minReplicas"
type AutoscalingSpec struct {
	// Enabled creates and manages an HPA for the Retriever Deployment.
	// +kubebuilder:default=false
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// MinReplicas is the lower replica bound.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`
	// MaxReplicas is the upper replica bound.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxReplicas int32 `json:"maxReplicas,omitempty"`
	// TargetCPUUtilizationPercentage is the average CPU utilization target.
	// +kubebuilder:default=70
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	TargetCPUUtilizationPercentage int32 `json:"targetCPUUtilizationPercentage,omitempty"`
}

// RetrieverIngressSpec exposes a Retriever through a Kubernetes Ingress.
type RetrieverIngressSpec struct {
	// Host is the public DNS name routed to the Retriever.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// ClassName selects the Ingress controller.
	// +optional
	ClassName string `json:"className,omitempty"`
	// Path is the URL path prefix.
	// +kubebuilder:default="/"
	// +kubebuilder:validation:Pattern=`^/`
	// +optional
	Path string `json:"path,omitempty"`
	// Annotations are copied to the managed Ingress.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// TLSSecretName enables TLS using this Secret. The Secret may be provisioned
	// by cert-manager when ClusterIssuer is set.
	// +optional
	TLSSecretName string `json:"tlsSecretName,omitempty"`
	// ClusterIssuer adds the cert-manager.io/cluster-issuer annotation.
	// +optional
	ClusterIssuer string `json:"clusterIssuer,omitempty"`
}

// OIDCSpec configures an oauth2-proxy sidecar in front of the Retriever.
type OIDCSpec struct {
	// IssuerURL is the OpenID Connect issuer discovery URL.
	// +kubebuilder:validation:Pattern=`^https://`
	IssuerURL string `json:"issuerURL"`
	// ClientIDSecretRef contains the OIDC client ID.
	ClientIDSecretRef SecretKeyRef `json:"clientIDSecretRef"`
	// ClientSecretSecretRef contains the OIDC client secret.
	ClientSecretSecretRef SecretKeyRef `json:"clientSecretSecretRef"`
	// CookieSecretRef contains an oauth2-proxy cookie secret. Use 16, 24, or 32
	// random bytes encoded as base64url.
	CookieSecretRef SecretKeyRef `json:"cookieSecretRef"`
	// EmailDomains restricts login to these domains. Empty defaults to "*".
	// +optional
	EmailDomains []string `json:"emailDomains,omitempty"`
	// Groups restricts login to users with at least one matching OIDC group.
	// +optional
	Groups []string `json:"groups,omitempty"`
	// Image overrides the oauth2-proxy image.
	// +kubebuilder:default="quay.io/oauth2-proxy/oauth2-proxy:v7.15.3"
	// +optional
	Image string `json:"image,omitempty"`
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
	// +kubebuilder:default=openai
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
// +kubebuilder:validation:XValidation:rule="!has(self.oidc) || has(self.ingress)",message="ingress is required when oidc is configured"
// +kubebuilder:validation:XValidation:rule="!(has(self.oidc) && has(self.apiKeySecretRef))",message="oidc and apiKeySecretRef cannot be enabled together"
type RetrieverSpec struct {
	// KnowledgeBaseRef names the KnowledgeBase to serve from (same namespace).
	KnowledgeBaseRef LocalObjectRef `json:"knowledgeBaseRef"`
	// APIKeySecretRef enables API-key authentication for the retriever. The
	// referenced Secret value is accepted as either an Authorization Bearer
	// token or an X-API-Key header. Health probes remain unauthenticated.
	// +optional
	APIKeySecretRef *SecretKeyRef `json:"apiKeySecretRef,omitempty"`
	// RateLimit enables per-client request throttling. Omit to disable.
	// +optional
	RateLimit *RateLimitSpec `json:"rateLimit,omitempty"`
	// MaxConcurrentRequests caps in-flight non-health requests per pod. Excess
	// requests receive 503 with Retry-After instead of exhausting the process.
	// +kubebuilder:default=32
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	// +optional
	MaxConcurrentRequests int `json:"maxConcurrentRequests,omitempty"`
	// MaxRequestBodyBytes rejects oversized request bodies before parsing.
	// +kubebuilder:default=1048576
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=104857600
	// +optional
	MaxRequestBodyBytes int64 `json:"maxRequestBodyBytes,omitempty"`
	// PodDisruptionBudget controls whether the operator maintains a PDB for this
	// Retriever. Enabled by default when replicas is greater than one.
	// +optional
	PodDisruptionBudget *bool `json:"podDisruptionBudget,omitempty"`
	// Autoscaling optionally creates a CPU-based HPA.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`
	// Ingress optionally exposes the Retriever outside the cluster.
	// +optional
	Ingress *RetrieverIngressSpec `json:"ingress,omitempty"`
	// OIDC enables an oauth2-proxy sidecar. Ingress is required and native
	// apiKeySecretRef authentication must not be enabled at the same time.
	// +optional
	OIDC *OIDCSpec `json:"oidc,omitempty"`
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
	// +kubebuilder:default=0
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
	// +kubebuilder:validation:Enum=Pending;Available;Progressing
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ServiceEndpoint is the in-cluster URL of the retriever, when available.
	// +optional
	ServiceEndpoint string `json:"serviceEndpoint,omitempty"`
	// PublicEndpoint is the managed Ingress URL, when configured.
	// +optional
	PublicEndpoint string `json:"publicEndpoint,omitempty"`
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
