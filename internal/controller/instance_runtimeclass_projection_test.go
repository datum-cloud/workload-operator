// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	locationsv1alpha1 "go.miloapis.com/locations/api/v1alpha1"
	"go.miloapis.com/milo/pkg/downstreamclient"
)

// rcTestClass is the class a cell stamps on the instances it runs.
const rcTestClass = "general-purpose"

// rcTestCellInstance builds a cell-plane Instance shaped after a live staging
// object. It carries every controller-managed label a cell stamps and a sandbox
// spec naming the resolved class. A fixture that drops production fields hides
// projection bugs.
func rcTestCellInstance(class string) *computev1alpha.Instance {
	labels := map[string]string{
		computev1alpha.InstanceIndexLabel:          projTestInstanceIndex,
		computev1alpha.LocationLabel:               testWestLocationName,
		computev1alpha.PlacementNameLabel:          testDefaultPlacement,
		computev1alpha.WorkloadDeploymentNameLabel: projTestWDName,
		computev1alpha.WorkloadDeploymentUIDLabel:  string(projTestEdgeWDUID),
		computev1alpha.WorkloadNameLabel:           "my-workload",
		computev1alpha.WorkloadUIDLabel:            projTestWorkloadUID,
		"resourcemanager.miloapis.com/service":     "compute.datumapis.com",
	}
	if class != "" {
		labels[computev1alpha.RuntimeClassLabel] = class
	}

	return &computev1alpha.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      projTestInstanceName,
			Namespace: projTestKarmadaNS,
			Labels:    labels,
		},
		Spec: computev1alpha.InstanceSpec{
			Runtime: computev1alpha.InstanceRuntimeSpec{
				Class: class,
				Resources: computev1alpha.InstanceRuntimeResources{
					InstanceType: testInstanceType,
				},
				Sandbox: &computev1alpha.SandboxRuntime{
					Containers: []computev1alpha.SandboxContainer{
						{
							Name:  "server",
							Image: "ghcr.io/acme/api:1.4.2",
							Resources: &computev1alpha.ContainerResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("4Gi"),
								},
							},
						},
					},
				},
			},
			NetworkInterfaces: []computev1alpha.InstanceNetworkInterface{
				{
					Name:    "eth0",
					Network: networkingv1alpha.NetworkRef{Name: "default"},
				},
			},
			Location: &locationsv1alpha1.LocationReference{Name: testWestLocationName},
			Controller: &computev1alpha.InstanceController{
				TemplateHash: "6c9f7d4b58",
			},
		},
		Status: computev1alpha.InstanceStatus{
			Conditions: []metav1.Condition{
				{
					Type:               computev1alpha.InstanceReady,
					Status:             metav1.ConditionTrue,
					Reason:             computev1alpha.InstanceReadyReasonAvailable,
					Message:            "Instance is ready",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
}

// rcTestFederationNamespace returns the federation-plane namespace the cell
// instance lives in, carrying the upstream identity labels write-back reads.
func rcTestFederationNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: projTestKarmadaNS,
			Labels: map[string]string{
				downstreamclient.UpstreamOwnerNamespaceLabel:   projTestProjNS,
				downstreamclient.UpstreamOwnerClusterNameLabel: encodedCluster(),
			},
		},
	}
}

// rcTestHubDeployment returns the hub WorkloadDeployment that owns write-back
// copies. Write-back refuses to create a copy without it.
func rcTestHubDeployment() *computev1alpha.WorkloadDeployment {
	return &computev1alpha.WorkloadDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      projTestWDName,
			Namespace: projTestKarmadaNS,
			UID:       projTestEdgeWDUID,
		},
	}
}

// TestRuntimeClassReachesProjectInstance walks the path a customer reads. A cell
// instance is written back to the hub, and the hub copy is projected into the
// project control plane. The class must survive both hops, or the project plane
// reports a runtime the cell is not running.
func TestRuntimeClassReachesProjectInstance(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	federationClient := fake.NewClientBuilder().
		WithScheme(newKarmadaScheme()).
		WithObjects(rcTestFederationNamespace(), rcTestHubDeployment()).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	reconciler := &InstanceReconciler{
		FederationClient: federationClient,
		scheme:           newKarmadaScheme(),
	}
	require.NoError(t, reconciler.writeBackToUpstream(ctx, rcTestCellInstance(rcTestClass)))

	var hubInstance computev1alpha.Instance
	require.NoError(t, federationClient.Get(ctx,
		types.NamespacedName{Namespace: projTestKarmadaNS, Name: projTestInstanceName},
		&hubInstance))
	assert.Equal(t, rcTestClass, hubInstance.Labels[computev1alpha.RuntimeClassLabel],
		"write-back must carry the runtime class label to the hub copy")

	projectClient := fake.NewClientBuilder().
		WithScheme(newProjectScheme()).
		WithObjects(projTestProjectNS(), projTestWorkloadDeployment()).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	projector := newTestProjector(federationClient, projectClient)
	_, err := projector.Reconcile(ctx, projectorRequest())
	require.NoError(t, err)

	var projection computev1alpha.Instance
	require.NoError(t, projectClient.Get(ctx,
		types.NamespacedName{Namespace: projTestProjNS, Name: projTestInstanceName},
		&projection))

	assert.Equal(t, rcTestClass, projection.Labels[computev1alpha.RuntimeClassLabel],
		"the projected instance must expose the runtime class the cell runs")
	assert.Equal(t, rcTestClass, projection.Spec.Runtime.Class,
		"the projected instance spec must keep the resolved class")
}

// TestWriteBackToUpstream_NoRuntimeClass_NoLabel verifies that an instance with
// no resolved class produces a hub copy with no class label. Naming a class
// there would assert a tier nothing chose.
func TestWriteBackToUpstream_NoRuntimeClass_NoLabel(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	federationClient := fake.NewClientBuilder().
		WithScheme(newKarmadaScheme()).
		WithObjects(rcTestFederationNamespace(), rcTestHubDeployment()).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	reconciler := &InstanceReconciler{
		FederationClient: federationClient,
		scheme:           newKarmadaScheme(),
	}
	require.NoError(t, reconciler.writeBackToUpstream(ctx, rcTestCellInstance("")))

	var hubInstance computev1alpha.Instance
	require.NoError(t, federationClient.Get(ctx,
		types.NamespacedName{Namespace: projTestKarmadaNS, Name: projTestInstanceName},
		&hubInstance))

	_, stamped := hubInstance.Labels[computev1alpha.RuntimeClassLabel]
	assert.False(t, stamped, "an unclassed instance must not gain a class label upstream")
}

// TestWriteBackToUpstream_RuntimeClassBackfill covers a hub copy that already
// exists without the class label. Write-back adds it on the update path.
func TestWriteBackToUpstream_RuntimeClassBackfill(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cellInstance := rcTestCellInstance(rcTestClass)

	// An existing hub copy with every label except the class.
	existing := cellInstance.DeepCopy()
	existing.Labels = map[string]string{
		downstreamclient.UpstreamOwnerNamespaceLabel:   projTestProjNS,
		downstreamclient.UpstreamOwnerClusterNameLabel: encodedCluster(),
		computev1alpha.InstanceIndexLabel:              projTestInstanceIndex,
		computev1alpha.LocationLabel:                   testWestLocationName,
		computev1alpha.PlacementNameLabel:              testDefaultPlacement,
		computev1alpha.WorkloadDeploymentNameLabel:     projTestWDName,
		computev1alpha.WorkloadDeploymentUIDLabel:      string(projTestEdgeWDUID),
		computev1alpha.WorkloadNameLabel:               "my-workload",
		computev1alpha.WorkloadUIDLabel:                projTestWorkloadUID,
	}

	federationClient := fake.NewClientBuilder().
		WithScheme(newKarmadaScheme()).
		WithObjects(rcTestFederationNamespace(), rcTestHubDeployment(), existing).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	reconciler := &InstanceReconciler{
		FederationClient: federationClient,
		scheme:           newKarmadaScheme(),
	}
	require.NoError(t, reconciler.writeBackToUpstream(ctx, cellInstance))

	var hubInstance computev1alpha.Instance
	require.NoError(t, federationClient.Get(ctx,
		types.NamespacedName{Namespace: projTestKarmadaNS, Name: projTestInstanceName},
		&hubInstance))
	assert.Equal(t, rcTestClass, hubInstance.Labels[computev1alpha.RuntimeClassLabel],
		"an existing hub copy must gain the class label on the update path")
}

// TestInstanceProjector_RuntimeClassLabelCopied guards the projector half of
// the path on its own. Narrowing the projected label set then fails here.
func TestInstanceProjector_RuntimeClassLabelCopied(t *testing.T) {
	t.Parallel()

	karmadaInstance := projTestKarmadaInstance(map[string]string{
		computev1alpha.RuntimeClassLabel: rcTestClass,
	})

	projectClient := fake.NewClientBuilder().
		WithScheme(newProjectScheme()).
		WithObjects(projTestProjectNS(), projTestWorkloadDeployment()).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()

	projector := newTestProjector(newKarmadaFakeClient(karmadaInstance), projectClient)
	_, err := projector.Reconcile(context.Background(), projectorRequest())
	require.NoError(t, err)

	var projection computev1alpha.Instance
	require.NoError(t, projectClient.Get(context.Background(),
		types.NamespacedName{Namespace: projTestProjNS, Name: projTestInstanceName},
		&projection))
	assert.Equal(t, rcTestClass, projection.Labels[computev1alpha.RuntimeClassLabel])
}
