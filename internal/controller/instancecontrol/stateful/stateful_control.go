package stateful

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"go.datum.net/workload-operator/api/v1alpha"
	"go.datum.net/workload-operator/internal/controller/instancecontrol"
)

// Behavior inspired by https://github.com/kubernetes/kubernetes/tree/master/pkg/controller/statefulset
// Does not currently implement exact behavior.
type statefulControl struct {
}

func NewStatefulControl() instancecontrol.Strategy {
	return &statefulControl{}
}

func (c *statefulControl) GetActions(
	ctx context.Context,
	scheme *runtime.Scheme,
	deployment *v1alpha.WorkloadDeployment,
	currentInstances []v1alpha.Instance,
) ([]instancecontrol.Action, error) {
	// lowest -> highest
	var createActions []instancecontrol.Action
	var waitActions []instancecontrol.Action

	// highest -> lowest
	var updateActions []instancecontrol.Action

	// highest -> lowest
	var deleteActions []instancecontrol.Action

	// Instances that are desired to exist. We do not currently support the
	// concept of a partition, so will fill the entire slice.
	desiredInstances := make([]*v1alpha.Instance, deployment.Spec.ScaleSettings.MinReplicas)

	for _, instance := range currentInstances {
		instanceIndex := getInstanceOrdinal(instance.Name)
		if instanceIndex >= len(desiredInstances) {
			deleteActions = append(deleteActions, instancecontrol.NewDeleteAction(
				instance.Name,
				func(ctx context.Context, c client.Client) error {
					return c.Delete(ctx, &instance)
				},
			))
		} else {
			desiredInstances[instanceIndex] = &instance
		}
	}

	// It's possible that the incoming currentInstances will have gaps in
	// instances, so fill them in.
	for i := range deployment.Spec.ScaleSettings.MinReplicas {
		if desiredInstances[i] == nil {
			desiredInstances[i] = &v1alpha.Instance{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      deployment.Spec.Template.Labels,
					Annotations: deployment.Spec.Template.Annotations,
					Name:        fmt.Sprintf("%s-%d", deployment.Name, i),
					Namespace:   deployment.Namespace,
				},
				Spec: deployment.Spec.Template.Spec,
			}

			if desiredInstances[i].Labels == nil {
				desiredInstances[i].Labels = map[string]string{}
			}

			desiredInstances[i].Labels[v1alpha.InstanceIndexLabel] = strconv.Itoa(int(i))

			if err := controllerutil.SetControllerReference(deployment, desiredInstances[i], scheme); err != nil {
				return nil, fmt.Errorf("failed to set controller reference: %w", err)
			}
		}
	}

	for _, instance := range desiredInstances {
		if instance.CreationTimestamp.IsZero() {
			action := instancecontrol.NewCreateAction(
				instance.Name,
				func(ctx context.Context, c client.Client) error {
					if err := c.Create(ctx, instance); err != nil {
						return fmt.Errorf("failed to create instance: %w", err)
					}

					return nil
				},
			)

			createActions = append(createActions, action)
		} else if !instance.DeletionTimestamp.IsZero() {
			// Wait for graceful deletion before continuing processing additional
			// instances.
			waitActions = append(waitActions, instancecontrol.NewWaitAction(
				instance.Name,
			))

		} else if instance.DeletionTimestamp.IsZero() {
			// Wait for the instance to be ready before continuing processing
			if !apimeta.IsStatusConditionTrue(instance.Status.Conditions, v1alpha.InstanceReady) {
				waitActions = append(waitActions, instancecontrol.NewWaitAction(
					instance.Name,
				))
			} else if needsUpdate(instance, deployment) {
				updateActions = append(updateActions, instancecontrol.NewUpdateAction(
					instance.Name,
					func(ctx context.Context, c client.Client) error {
						instance.Annotations = deployment.Spec.Template.Annotations
						instance.Labels = deployment.Spec.Template.Labels
						instance.Spec = deployment.Spec.Template.Spec

						if err := c.Update(ctx, instance); err != nil {
							return fmt.Errorf("failed to update instance: %w", err)
						}

						return nil
					},
				))
			}
		}
	}

	slices.SortFunc(updateActions, descendingOrdinal)
	slices.SortFunc(deleteActions, descendingOrdinal)

	actions := make([]instancecontrol.Action, 0, len(createActions)+len(waitActions)+len(updateActions)+len(deleteActions))

	switch deployment.Spec.ScaleSettings.InstanceManagementPolicy {
	case v1alpha.OrderedReadyInstanceManagementPolicyType:

		// Add create and wait actions, and sort by ordinal. This allows us to wait
		// for instances to be processed in the correct order.
		//
		// For instance, we may have instance 0 that needs to wait to be ready, but
		// instance 1 wants to be created.
		actions = append(actions, createActions...)
		actions = append(actions, waitActions...)

		slices.SortFunc(actions, ascendingOrdinal)

		actions = append(actions, updateActions...)
		actions = append(actions, deleteActions...)

		// Skip all actions except the first one.
		for i := range actions {
			if i > 0 {
				actions[i].SkipExecution()
			}
		}

	}

	return actions, nil
}
