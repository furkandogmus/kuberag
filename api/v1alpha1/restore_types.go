package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RestorePhase represents the lifecycle phase of a Restore.
// +kubebuilder:validation:Enum=Pending;Running;Completed;Failed
type RestorePhase string

const (
	RestorePhasePending   RestorePhase = "Pending"
	RestorePhaseRunning   RestorePhase = "Running"
	RestorePhaseCompleted RestorePhase = "Completed"
	RestorePhaseFailed    RestorePhase = "Failed"
)

// RestoreSpec defines the desired state of Restore.
type RestoreSpec struct {
	// BackupRef references the Backup resource to restore from. Must exist
	// in the same namespace.
	// +kubebuilder:validation:Required
	BackupRef LocalObjectRef `json:"backupRef"`

	// KnowledgeBaseRef points to the KnowledgeBase that will receive the
	// restored data. Its vector store collection will be overwritten.
	// +kubebuilder:validation:Required
	KnowledgeBaseRef LocalObjectRef `json:"knowledgeBaseRef"`

	// Suspend, when true, pauses the restore controller. Already-running
	// restore Jobs are not affected.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// RestoreStatus defines the observed state of Restore.
type RestoreStatus struct {
	// Phase of the restore lifecycle.
	// +optional
	Phase RestorePhase `json:"phase,omitempty"`

	// ActiveJob is the name of the in-flight restore Job, if any.
	// +optional
	ActiveJob string `json:"activeJob,omitempty"`

	// CompletionTime is when the restore finished successfully.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// RestoredPoints is the number of vector points restored.
	// +optional
	RestoredPoints int `json:"restoredPoints,omitempty"`

	// Conditions represent the current state of the Restore.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Backup",type="string",JSONPath=".spec.backupRef.name"
// +kubebuilder:printcolumn:name="KB",type="string",JSONPath=".spec.knowledgeBaseRef.name"
// +kubebuilder:printcolumn:name="Points",type="integer",JSONPath=".status.restoredPoints"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Restore recovers a KnowledgeBase's vector store from a Backup snapshot.
type Restore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RestoreSpec   `json:"spec,omitempty"`
	Status RestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RestoreList contains a list of Restores.
type RestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Restore `json:"items"`
}
