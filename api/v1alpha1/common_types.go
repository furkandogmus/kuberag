package v1alpha1

// SecretKeyRef points at a single key inside a Secret in the same namespace.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// LocalObjectRef references another object by name in the same namespace.
type LocalObjectRef struct {
	Name string `json:"name"`
}

// Condition types shared across the API.
const (
	// ConditionReady is the top-level readiness of a resource.
	ConditionReady = "Ready"
	// ConditionIngesting is set while an ingestion Job is in flight.
	ConditionIngesting = "Ingesting"
	// ConditionEvaluated tracks the last retrieval-quality evaluation.
	ConditionEvaluated = "Evaluated"
	// ConditionAvailable tracks serving availability for Retrievers.
	ConditionAvailable = "Available"
)
