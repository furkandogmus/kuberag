// Package v1alpha1 contains API Schema definitions for the rag v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=rag.furkan.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "rag.furkan.dev", Version: "v1alpha1"}

	// SchemeBuilder collects the functions that add this group's types to a Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&KnowledgeBase{}, &KnowledgeBaseList{},
		&Retriever{}, &RetrieverList{},
		&VectorIndex{}, &VectorIndexList{},
		&IngestionRun{}, &IngestionRunList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
