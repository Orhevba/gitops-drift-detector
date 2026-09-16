// Package v1alpha1 contains the WatchedRepo API.
// +kubebuilder:object:generate=true
// +groupName=drift.tygacookie.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the API group and version used to register these types.
	GroupVersion = schema.GroupVersion{Group: "drift.tygacookie.dev", Version: "v1alpha1"}

	// SchemeBuilder registers these types with a runtime.Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds this group's types to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &WatchedRepo{}, &WatchedRepoList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
