package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IngestionRunPhase is the high-level state of an ingestion attempt.
// +kubebuilder:validation:Enum=Running;Succeeded;Failed
type IngestionRunPhase string

const (
	IngestionRunRunning   IngestionRunPhase = "Running"
	IngestionRunSucceeded IngestionRunPhase = "Succeeded"
	IngestionRunFailed    IngestionRunPhase = "Failed"
)

// IngestionRunSpec captures the configuration that triggered this run.
type IngestionRunSpec struct {
	// KnowledgeBaseRef names the owning KnowledgeBase.
	KnowledgeBaseRef LocalObjectRef `json:"knowledgeBaseRef"`
	// Mode is the ingestion mode used (full or incremental).
	Mode IngestMode `json:"mode"`
	// SpecHash is the spec fingerprint at the time the run started.
	SpecHash string `json:"specHash"`
	// EffectiveChunking is the chunking config used for this run.
	EffectiveChunking ChunkingSpec `json:"effectiveChunking"`
}

// IngestionRunStatus records the outcome of this run.
type IngestionRunStatus struct {
	// Phase is the terminal state.
	Phase IngestionRunPhase `json:"phase,omitempty"`
	// StartTime is when the run was created.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// CompletionTime is when the run finished (success or failure).
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// TotalChunks is the number of chunks indexed by this run.
	// +optional
	TotalChunks int `json:"totalChunks,omitempty"`
	// Sources records per-source ingestion results.
	// +optional
	Sources []SourceStatus `json:"sources,omitempty"`
	// Error holds the failure reason when Phase=Failed.
	// +optional
	Error string `json:"error,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ir
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || self.status.phase == 'Running' || oldSelf.status.phase == self.status.phase || oldSelf.status.phase == 'Running'",message="spec is immutable; only status.phase may transition from Running to Succeeded/Failed"
// +kubebuilder:printcolumn:name="KB",type=string,JSONPath=`.spec.knowledgeBaseRef.name`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Chunks",type=integer,JSONPath=`.status.totalChunks`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IngestionRun is an immutable record of a single ingestion attempt.
// Created by the KnowledgeBase controller at the start of each ingest and
// updated with the result. Owned by the KnowledgeBase for garbage collection.
type IngestionRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IngestionRunSpec   `json:"spec,omitempty"`
	Status IngestionRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IngestionRunList contains a list of IngestionRun.
type IngestionRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IngestionRun `json:"items"`
}
