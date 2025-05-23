// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"slices"

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

	"go.datum.net/workload-operator/internal/controller/instancecontrol"
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

	if deployment.Status.Location == nil {
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

	result := r.reconcileNetworks(ctx, cl.GetClient(), &deployment)
	if result.ShouldReturn() {
		if result.Err == nil {
			// TODO(jreese) update instance status conditions to communicate what
			// we're waiting on.
		}

		return result.Complete(ctx)
	}

	// Networks are all ready with subnets ready to use, remove any scheduling
	// gates on instances. If any instances were created by actions above, those
	// will result in this reconciler being processed again, so will properly have
	// their gates removed.

	for _, instance := range instances.Items {
		if len(instance.Spec.Controller.SchedulingGates) == 0 {
			continue
		}

		newGates := slices.DeleteFunc(instance.Spec.Controller.SchedulingGates, func(gate computev1alpha.SchedulingGate) bool {
			return gate.Name == instancecontrol.NetworkSchedulingGate.String()
		})

		if len(newGates) == len(instance.Spec.Controller.SchedulingGates) {
			// Already been removed, there must be other gates.
			continue
		}

		instance.Spec.Controller.SchedulingGates = newGates

		// TODO(jreese) consider using patches
		if err := cl.GetClient().Update(ctx, &instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed updating instance: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *WorkloadDeploymentReconciler) reconcileNetworks(
	ctx context.Context,
	c client.Client,
	deployment *computev1alpha.WorkloadDeployment,
) (result Result) {
	logger := log.FromContext(ctx)

	// First, ensure we have a NetworkBinding for each interface, and that the
	// binding is ready before we move on to create SubnetClaims.

	var networkContextRefs []networkingv1alpha.NetworkContextRef
	allNetworkBindingsReady := true
	for i, networkInterface := range deployment.Spec.Template.Spec.NetworkInterfaces {
		var networkBinding networkingv1alpha.NetworkBinding
		networkBindingObjectKey := client.ObjectKey{
			Namespace: deployment.Namespace,
			Name:      fmt.Sprintf("%s-net-%d", deployment.Name, i),
		}

		if err := c.Get(ctx, networkBindingObjectKey, &networkBinding); client.IgnoreNotFound(err) != nil {
			result.Err = fmt.Errorf("failed checking for existing network binding: %w", err)
			return result
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

			if err := controllerutil.SetControllerReference(deployment, &networkBinding, c.Scheme()); err != nil {
				result.Err = fmt.Errorf("failed to set controller on network binding: %w", err)
				return result
			}

			if err := c.Create(ctx, &networkBinding); err != nil {
				result.Err = fmt.Errorf("failed creating network binding: %w", err)
				return result
			}
		}

		if !apimeta.IsStatusConditionTrue(networkBinding.Status.Conditions, networkingv1alpha.NetworkBindingReady) {
			allNetworkBindingsReady = false
		} else if networkBinding.Status.NetworkContextRef != nil {
			networkContextRefs = append(networkContextRefs, *networkBinding.Status.NetworkContextRef)
		}
	}

	if !allNetworkBindingsReady {
		logger.Info("waiting for network bindings to be ready")
		result.StopProcessing = true
		return result
	}

	// TODO(jreese): Currently this makes a SubnetClaim that will be used by
	// many instances. Move to a claim per instance interface, and allocate from
	// a larger subnet. In addition, it does not handle allocation of more than
	// one subnet per network context. We'll have a future IPAM controller in
	// network-services-operator that will handle this.
	//
	// Also, only handling ipv4

	for _, networkContextRef := range networkContextRefs {
		var networkContext networkingv1alpha.NetworkContext
		networkContextObjectKey := client.ObjectKey{
			Namespace: networkContextRef.Namespace,
			Name:      networkContextRef.Name,
		}

		if err := c.Get(ctx, networkContextObjectKey, &networkContext); client.IgnoreNotFound(err) != nil {
			result.Err = fmt.Errorf("failed checking for existing network context: %w", err)
			return result
		}

		if !apimeta.IsStatusConditionTrue(networkContext.Status.Conditions, networkingv1alpha.NetworkContextReady) {
			logger.Info("waiting for network context to be ready", "network_context", networkContext.Name)
			result.StopProcessing = true
			return result
		}

		var subnetClaims networkingv1alpha.SubnetClaimList
		listOpts := []client.ListOption{
			client.InNamespace(networkContext.Namespace),
		}

		if err := c.List(ctx, &subnetClaims, listOpts...); err != nil {
			result.Err = fmt.Errorf("failed listing subnet claims: %w", err)
			return result
		}

		var subnetClaim networkingv1alpha.SubnetClaim
		for _, claim := range subnetClaims.Items {
			// If it's not the same subnet class, don't consider the subnet claim.
			if claim.Spec.SubnetClass != "private" {
				continue
			}

			// If it's not ipv4, don't consider the subnet claim.
			if claim.Spec.IPFamily != networkingv1alpha.IPv4Protocol {
				continue
			}

			// If it's not the same network context, don't consider the subnet claim.
			if claim.Spec.NetworkContext.Name != networkContext.Name {
				continue
			}

			// If it's not the same location, don't consider the subnet claim.
			if claim.Spec.Location.Namespace != deployment.Status.Location.Namespace ||
				claim.Spec.Location.Name != deployment.Status.Location.Name {
				continue
			}

			subnetClaim = claim
			break
		}

		if subnetClaim.CreationTimestamp.IsZero() {
			subnetClaim = networkingv1alpha.SubnetClaim{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: networkContext.Namespace,
					// In the future, subnets will be created with an ordinal that increases.
					// This ensures that we don't create duplicate subnet claims when the
					// cache is not up to date.
					Name: fmt.Sprintf("%s-0", networkContext.Name),
				},
				Spec: networkingv1alpha.SubnetClaimSpec{
					SubnetClass: "private",
					IPFamily:    networkingv1alpha.IPv4Protocol,
					NetworkContext: networkingv1alpha.LocalNetworkContextRef{
						Name: networkContext.Name,
					},
					Location: *deployment.Status.Location,
				},
			}

			if err := controllerutil.SetOwnerReference(&networkContext, &subnetClaim, c.Scheme()); err != nil {
				result.Err = fmt.Errorf("failed to set controller on subnet claim: %w", err)
				return result
			}

			if err := c.Create(ctx, &subnetClaim); err != nil {
				result.Err = fmt.Errorf("failed creating subnet claim: %w", err)
				return result
			}

			logger.Info("created subnet claim", "subnetClaim", subnetClaim.Name)

			result.StopProcessing = true
			return result
		}

		logger.Info("found subnet claim", "subnetClaim", subnetClaim.Name)

		if !apimeta.IsStatusConditionTrue(subnetClaim.Status.Conditions, "Ready") {
			logger.Info("waiting for subnet claim to be ready", "subnetClaim", subnetClaim.Name)
			result.StopProcessing = true
			return result
		}

		var subnet networkingv1alpha.Subnet
		subnetObjectKey := client.ObjectKey{
			Namespace: subnetClaim.Namespace,
			Name:      subnetClaim.Status.SubnetRef.Name,
		}
		if err := c.Get(ctx, subnetObjectKey, &subnet); err != nil {
			result.Err = fmt.Errorf("failed fetching subnet: %w", err)
			return result
		}

		if !apimeta.IsStatusConditionTrue(subnet.Status.Conditions, "Ready") {
			logger.Info("waiting for subnet to be ready", "subnet", subnet.Name)
			result.StopProcessing = true
			return result
		}

		logger.Info("found subnet", "subnet", subnet.Name)

	}

	return result
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadDeploymentReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr
	// TODO(jreese) finalizers
	// TODO(jreese) watch subnet claims and subnets, enqueue based on location
	// match on workload deployment.
	return mcbuilder.ControllerManagedBy(mgr).
		For(&computev1alpha.WorkloadDeployment{}, mcbuilder.WithEngageWithLocalCluster(false)).
		Owns(&computev1alpha.Instance{}).
		Owns(&networkingv1alpha.NetworkBinding{}).
		Complete(r)
}
