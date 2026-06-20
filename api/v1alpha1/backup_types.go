package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BackupPhase represents the lifecycle phase of a Backup.
// +kubebuilder:validation:Enum=Pending;Running;Completed;Failed
type BackupPhase string

const (
	BackupPhasePending   BackupPhase = "Pending"
	BackupPhaseRunning   BackupPhase = "Running"
	BackupPhaseCompleted BackupPhase = "Completed"
	BackupPhaseFailed    BackupPhase = "Failed"
)

// BackupSpec defines the desired state of Backup.
type BackupSpec struct {
	// KnowledgeBaseRef points to the KnowledgeBase whose vector store data
	// should be backed up. Must exist in the same namespace.
	// +kubebuilder:validation:Required
	KnowledgeBaseRef LocalObjectRef `json:"knowledgeBaseRef"`

	// Destination configures where the backup archive is stored.
	// +kubebuilder:validation:Required
	Destination BackupDestination `json:"destination"`

	// Suspend, when true, pauses the backup controller. Already-running
	// backup Jobs are not affected.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// BackupDestination specifies the target object storage for a backup snapshot.
// +kubebuilder:validation:XValidation:rule="has(self.s3)",message="s3 block is required"
type BackupDestination struct {
	// S3 configures an S3-compatible destination for the backup archive.
	// +kubebuilder:validation:Required
	S3 *S3BackupTarget `json:"s3"`
}

// S3BackupTarget holds the S3-compatible destination details.
type S3BackupTarget struct {
	// Endpoint is the S3-compatible server URL (e.g. https://s3.amazonaws.com).
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// Region for the S3 bucket.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`

	// Bucket to store the backup archive in.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Prefix within the bucket (e.g. "kuberag/backups"). Defaults to
	// "kuberag-backups".
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// AccessKeySecretRef references a Secret key holding the S3 access key.
	// +kubebuilder:validation:Required
	AccessKeySecretRef SecretKeyRef `json:"accessKeySecretRef"`

	// SecretKeySecretRef references a Secret key holding the S3 secret key.
	// +kubebuilder:validation:Required
	SecretKeySecretRef SecretKeyRef `json:"secretKeySecretRef"`
}

// BackupStatus defines the observed state of Backup.
type BackupStatus struct {
	// Phase of the backup lifecycle.
	// +optional
	Phase BackupPhase `json:"phase,omitempty"`

	// ActiveJob is the name of the in-flight backup Job, if any.
	// +optional
	ActiveJob string `json:"activeJob,omitempty"`

	// CompletionTime is when the backup finished successfully.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// BackupID is a unique identifier for this backup snapshot (timestamp-suffix).
	// +optional
	BackupID string `json:"backupID,omitempty"`

	// Location is the S3 URI where the backup archive was written.
	// +optional
	Location string `json:"location,omitempty"`

	// TotalPoints is the number of vector points included in the backup.
	// +optional
	TotalPoints int `json:"totalPoints,omitempty"`

	// SizeBytes is the approximate size of the backup archive in bytes.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// Conditions represent the current state of the Backup.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="KB",type="string",JSONPath=".spec.knowledgeBaseRef.name"
// +kubebuilder:printcolumn:name="Points",type="integer",JSONPath=".status.totalPoints"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Backup represents a point-in-time snapshot of a KnowledgeBase's vector store
// data, written to S3-compatible object storage.
type Backup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupSpec   `json:"spec,omitempty"`
	Status BackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BackupList contains a list of Backups.
type BackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Backup `json:"items"`
}
