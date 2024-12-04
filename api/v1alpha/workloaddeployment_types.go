package v1alpha

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// WorkloadDeploymentSpec defines the desired state of WorkloadDeployment
type WorkloadDeploymentSpec struct {
	// The workload that a deployment belongs to
	//
	// +kubebuilder:validation:Required
	WorkloadRef WorkloadReference `json:"workloadRef"`

	// The placement in the workload which is driving a deployment
	//
	// +kubebuilder:validation:Required
	PlacementName string `json:"placementName"`

	// TODO(jreese) think through how to structure this a bit better for when
	// deployments can be scheduled in ways other than just a city code.
	//
	// +kubebuilder:validation:Required
	CityCode string `json:"cityCode"`

	// Defines settings for each instance.
	//
	// +kubebuilder:validation:Required
	Template InstanceTemplateSpec `json:"template,omitempty"`

	// Scale settings such as minimum and maximum replica counts.
	//
	// +kubebuilder:validation:Required
	ScaleSettings HorizontalScaleSettings `json:"scaleSettings"`
}

// WorkloadDeploymentStatus defines the observed state of WorkloadDeployment
type WorkloadDeploymentStatus struct {
	// The cluster which the deployment has been assigned to
	//
	// +kubebuilder:validation:Optional
	ClusterRef *networkingv1alpha.DatumClusterReference `json:"clusterRef,omitempty"`

	// Represents the observations of a deployment's current state.
	// Known condition types are: "Available", "Progressing"
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// The number of instances created by a deployment
	Replicas int32 `json:"replicas"`

	// The number of instances created by a deployment and have the latest
	// deployment generation settings applied.
	CurrentReplicas int32 `json:"currentReplicas"`

	// The desired number of instances to be managed by a deployment.
	DesiredReplicas int32 `json:"desiredReplicas"`

	// TODO(jreese) ReadyReplicas?
}

const (
	// WorkloadDeploymentAvailable indicates that at least one instance has come
	// online.
	WorkloadDeploymentAvailable = "Available"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// WorkloadDeployment is the Schema for the workloaddeployments API
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.status.clusterRef.name`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].reason`
type WorkloadDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadDeploymentSpec   `json:"spec,omitempty"`
	Status WorkloadDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadDeploymentList contains a list of WorkloadDeployment
type WorkloadDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkloadDeployment{}, &WorkloadDeploymentList{})
}
