package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VectorIndexSpec describes a managed collection in a vector store. The operator
// creates one VectorIndex per KnowledgeBase to track collection health and size
// independently of the ingestion lifecycle.
type VectorIndexSpec struct {
	// KnowledgeBaseRef names the owning KnowledgeBase (same namespace).
	KnowledgeBaseRef LocalObjectRef  `json:"knowledgeBaseRef"`
	Store            VectorStoreSpec `json:"store"`
	// Dimension is the expected vector dimension for the collection.
	Dimension int `json:"dimension"`
	// ProbeIntervalSeconds controls how often health is checked.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=10
	// +optional
	ProbeIntervalSeconds int `json:"probeIntervalSeconds,omitempty"`
	// ProbeTimeoutSeconds is the HTTP request timeout for collection health probes.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +optional
	ProbeTimeoutSeconds int `json:"probeTimeoutSeconds,omitempty"`
}

// VectorIndexHealth is the observed health of the collection.
// +kubebuilder:validation:Enum=Healthy;Degraded;Missing;Unknown
type VectorIndexHealth string

const (
	IndexHealthy  VectorIndexHealth = "Healthy"
	IndexDegraded VectorIndexHealth = "Degraded"
	IndexMissing  VectorIndexHealth = "Missing"
	IndexUnknown  VectorIndexHealth = "Unknown"
)

// VectorIndexStatus is the observed collection state.
type VectorIndexStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Health VectorIndexHealth `json:"health,omitempty"`
	// +optional
	PointCount int64 `json:"pointCount,omitempty"`
	// +optional
	Dimension int `json:"dimension,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vi
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health`
// +kubebuilder:printcolumn:name="Points",type=integer,JSONPath=`.status.pointCount`
// +kubebuilder:printcolumn:name="Dim",type=integer,JSONPath=`.status.dimension`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VectorIndex tracks the health and size of a vector store collection.
type VectorIndex struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VectorIndexSpec   `json:"spec,omitempty"`
	Status VectorIndexStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VectorIndexList contains a list of VectorIndex.
type VectorIndexList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VectorIndex `json:"items"`
}
