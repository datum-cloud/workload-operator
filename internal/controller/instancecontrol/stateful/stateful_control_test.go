package stateful

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"go.datum.net/workload-operator/api/v1alpha"
	"go.datum.net/workload-operator/internal/controller/instancecontrol"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	v1alpha.AddToScheme(scheme)
}

func TestFreshDeployment(t *testing.T) {
	ctx := context.Background()
	control := NewStatefulControl()

	deployment := getWorkloadDeployment("test-deploy", "default", 2)

	// No instances
	var currentInstances []v1alpha.Instance
	actions, err := control.GetActions(ctx, scheme, deployment, currentInstances)

	assert.NoError(t, err)
	assert.Len(t, actions, 2)

	assert.Equal(t, "test-deploy-0", actions[0].ObjectName)
	assert.Equal(t, instancecontrol.ActionTypeCreate, actions[0].ActionType())
	assert.False(t, actions[0].IsSkipped())

	assert.Equal(t, "test-deploy-1", actions[1].ObjectName)
	assert.Equal(t, instancecontrol.ActionTypeCreate, actions[1].ActionType())
	assert.True(t, actions[1].IsSkipped())
}

func TestUpdateWithAllReadyInstances(t *testing.T) {
	ctx := context.Background()
	control := NewStatefulControl()

	var currentInstances []v1alpha.Instance
	currentInstances = append(currentInstances, *getInstance("test-deploy", "default", 0))
	currentInstances = append(currentInstances, *getInstance("test-deploy", "default", 1))

	deployment := getWorkloadDeployment("test-deploy", "default", 2)
	deployment.Spec.Template.Spec.Runtime.Sandbox.Containers[0].Image = "test-image-update"

	actions, err := control.GetActions(ctx, scheme, deployment, currentInstances)

	assert.NoError(t, err)
	assert.Len(t, actions, 2)

	assert.Equal(t, "test-deploy-1", actions[0].ObjectName)
	assert.Equal(t, instancecontrol.ActionTypeUpdate, actions[0].ActionType())
	assert.False(t, actions[0].IsSkipped())

	assert.Equal(t, "test-deploy-0", actions[1].ObjectName)
	assert.Equal(t, instancecontrol.ActionTypeUpdate, actions[1].ActionType())
	assert.True(t, actions[1].IsSkipped())
}

func TestScaleUpWithNotReadyInstance(t *testing.T) {
	ctx := context.Background()
	control := NewStatefulControl()

	var currentInstances []v1alpha.Instance
	currentInstances = append(currentInstances, *getInstance("test-deploy", "default", 0))

	notReadyInstance := getInstance("test-deploy", "default", 1)
	apimeta.SetStatusCondition(&notReadyInstance.Status.Conditions, metav1.Condition{
		Type:   v1alpha.InstanceReady,
		Status: metav1.ConditionFalse,
	})
	currentInstances = append(currentInstances, *notReadyInstance)

	deployment := getWorkloadDeployment("test-deploy", "default", 3)

	actions, err := control.GetActions(ctx, scheme, deployment, currentInstances)

	assert.NoError(t, err)
	assert.Len(t, actions, 2)

	assert.Equal(t, "test-deploy-1", actions[0].ObjectName)
	assert.Equal(t, instancecontrol.ActionTypeWait, actions[0].ActionType())
	assert.False(t, actions[0].IsSkipped())

	assert.Equal(t, "test-deploy-2", actions[1].ObjectName)
	assert.Equal(t, instancecontrol.ActionTypeCreate, actions[1].ActionType())
	assert.True(t, actions[1].IsSkipped())
}

func TestScaleDownWithAllReadyInstances(t *testing.T) {
	ctx := context.Background()
	control := NewStatefulControl()

	var currentInstances []v1alpha.Instance
	currentInstances = append(currentInstances, *getInstance("test-deploy", "default", 0))
	currentInstances = append(currentInstances, *getInstance("test-deploy", "default", 1))

	deployment := getWorkloadDeployment("test-deploy", "default", 1)

	actions, err := control.GetActions(ctx, scheme, deployment, currentInstances)

	assert.NoError(t, err)
	assert.Len(t, actions, 1)

	assert.Equal(t, "test-deploy-1", actions[0].ObjectName)
	assert.Equal(t, instancecontrol.ActionTypeDelete, actions[0].ActionType())
	assert.False(t, actions[0].IsSkipped())
}

// Add more test functions below for different scenarios.

func getWorkloadDeployment(name, namespace string, minReplicas int32) *v1alpha.WorkloadDeployment {
	instance := getInstance(name, namespace, 0)
	instance.Labels = nil
	deployment := &v1alpha.WorkloadDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1alpha.WorkloadDeploymentSpec{
			ScaleSettings: v1alpha.HorizontalScaleSettings{
				MinReplicas:              minReplicas,
				InstanceManagementPolicy: v1alpha.OrderedReadyInstanceManagementPolicyType,
			},
			Template: v1alpha.InstanceTemplateSpec{
				ObjectMeta: instance.ObjectMeta,
				Spec:       instance.Spec,
			},
		},
	}

	return deployment
}

func getInstance(name, namespace string, ordinal int) *v1alpha.Instance {
	instanceName := fmt.Sprintf("%s-%d", name, ordinal)
	instance := &v1alpha.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name:              instanceName,
			Namespace:         namespace,
			CreationTimestamp: metav1.Now(),
			Labels: map[string]string{
				v1alpha.InstanceIndexLabel: strconv.Itoa(ordinal),
			},
		},
		Spec: v1alpha.InstanceSpec{
			Runtime: v1alpha.InstanceRuntimeSpec{
				Resources: v1alpha.InstanceRuntimeResources{
					InstanceType: "datumcloud/d1-standard-2",
				},
				Sandbox: &v1alpha.SandboxRuntime{
					Containers: []v1alpha.SandboxContainer{
						{
							Name:  "test",
							Image: "test",
						},
					},
				},
			},
		},
		Status: v1alpha.InstanceStatus{
			Conditions: []metav1.Condition{
				{
					Type:   v1alpha.InstanceReady,
					Status: metav1.ConditionTrue,
				},
			},
		},
	}

	return instance
}
