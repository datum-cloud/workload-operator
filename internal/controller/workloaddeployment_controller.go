// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	computev1alpha "go.datum.net/workload-operator/api/v1alpha"

	instancecontrolstateful "go.datum.net/workload-operator/internal/controller/instancecontrol/stateful"
)

// WorkloadDeploymentReconciler reconciles a WorkloadDeployment object
type WorkloadDeploymentReconciler struct {
	mgr mcmanager.Manager
}

// +kubebuilder:rbac:groups=compute.datumapis.com,resources=workloaddeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=workloaddeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=workloaddeployments/finalizers,verbs=update

func (r *WorkloadDeploymentReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)

	var deployment computev1alpha.WorkloadDeployment
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !deployment.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling deployment")
	defer logger.Info("reconcile complete")

	// Ensure that a `NetworkBinding` is created for each network interface's
	// network.

	if deployment.Status.Location == nil {
		return ctrl.Result{}, nil
	}

	allNetworkBindingsReady := true
	for i, networkInterface := range deployment.Spec.Template.Spec.NetworkInterfaces {
		var networkBinding networkingv1alpha.NetworkBinding
		networkBindingObjectKey := client.ObjectKey{
			Namespace: deployment.Namespace,
			Name:      fmt.Sprintf("%s-net-%d", deployment.Name, i),
		}

		if err := cl.GetClient().Get(ctx, networkBindingObjectKey, &networkBinding); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("failed checking for existing network binding: %w", err)
		}

		if networkBinding.CreationTimestamp.IsZero() {
			networkBinding = networkingv1alpha.NetworkBinding{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: networkBindingObjectKey.Namespace,
					Name:      networkBindingObjectKey.Name,
				},
				Spec: networkingv1alpha.NetworkBindingSpec{
					Network:  networkInterface.Network,
					Location: *deployment.Status.Location,
				},
			}

			if err := controllerutil.SetControllerReference(&deployment, &networkBinding, cl.GetScheme()); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to set controller on network binding: %w", err)
			}

			if err := cl.GetClient().Create(ctx, &networkBinding); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed creating network binding: %w", err)
			}
		}

		if !apimeta.IsStatusConditionTrue(networkBinding.Status.Conditions, networkingv1alpha.NetworkBindingReady) {
			allNetworkBindingsReady = false
		}
	}

	if !allNetworkBindingsReady {
		logger.Info("waiting for network bindings to be ready")
		return ctrl.Result{}, nil
	}

	// Collect all instances for this deployment
	listOpts := client.MatchingLabels{
		computev1alpha.WorkloadDeploymentUIDLabel: string(deployment.GetUID()),
	}

	var instances computev1alpha.InstanceList
	if err := cl.GetClient().List(ctx, &instances, listOpts); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed listing instances: %w", err)
	}

	instanceControl := instancecontrolstateful.New()

	actions, err := instanceControl.GetActions(ctx, cl.GetScheme(), &deployment, instances.Items)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed getting instance control actions: %w", err)
	}

	logger.Info("collected instance control actions", "count", len(actions))

	for _, action := range actions {
		// We don't need to actually check this, but it'll reduce log noise.
		if action.IsSkipped() {
			continue
		}

		logger.Info("instance control action", "instance", action.Object.GetName(), "action", action.ActionType())

		if err := action.Execute(ctx, cl.GetClient()); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed executing instance control action: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadDeploymentReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr
	// TODO(jreese) finalizers
	return mcbuilder.ControllerManagedBy(mgr).
		For(&computev1alpha.WorkloadDeployment{}, mcbuilder.WithEngageWithLocalCluster(false)).
		Owns(&computev1alpha.Instance{}).
		Owns(&networkingv1alpha.NetworkBinding{}).
		Complete(r)
}
