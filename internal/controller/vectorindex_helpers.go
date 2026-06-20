package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

// ensureVectorIndex creates/updates the VectorIndex tracking this KB's collection.
func (r *KnowledgeBaseReconciler) ensureVectorIndex(ctx context.Context, kb *ragv1alpha1.KnowledgeBase) error {
	name := truncName(kb.Name + "-index")
	desired := &ragv1alpha1.VectorIndex{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: kb.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, desired, func() error {
		desired.Spec.KnowledgeBaseRef = ragv1alpha1.LocalObjectRef{Name: kb.Name}
		desired.Spec.Store = kb.Spec.VectorStore
		if desired.Spec.Store.Collection == "" {
			desired.Spec.Store.Collection = kb.Name
		}
		desired.Spec.Dimension = embeddingDimension(kb.Spec.Embedding)
		if desired.Spec.ProbeIntervalSeconds == 0 {
			desired.Spec.ProbeIntervalSeconds = 60
		}
		return controllerutil.SetControllerReference(kb, desired, r.Scheme)
	})
	return err
}

// embeddingDimension resolves the expected vector dimension for an embedding
// spec: explicit override first, then a built-in table of known models, then a
// sensible local default. Returns 0 for unknown hosted models, signalling the
// worker to auto-detect and the VectorIndex probe to skip the dimension check.
func embeddingDimension(e ragv1alpha1.EmbeddingSpec) int {
	if e.Dimension > 0 {
		return e.Dimension
	}
	switch e.Model {
	case "bge-small":
		return 384
	case "bge-large":
		return 1024
	case "text-embedding-004", "text-embedding-005":
		return 768
	case "text-embedding-3-small":
		return 1536
	case "text-embedding-3-large", "gemini-embedding-001":
		return 3072
	}
	if e.Provider == "" || e.Provider == "local" {
		return 384 // fastembed default
	}
	return 0 // hosted, unknown -> auto-detected at ingest time
}
