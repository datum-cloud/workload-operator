// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
)

// InstanceReconciler reconciles an Instance object
type InstanceReconciler struct {
	mgr mcmanager.Manager
}

// +kubebuilder:rbac:groups=compute.datumapis.com,resources=instances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=instances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=instances/finalizers,verbs=update

func (r *InstanceReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctx = mccontext.WithCluster(ctx, req.ClusterName)
	var instance computev1alpha.Instance
	if err := cl.GetClient().Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling instance")
	defer logger.Info("reconcile complete")

	// TODO(jreese) better condition handling

	if len(instance.Spec.Controller.SchedulingGates) > 0 {
		// Update Ready condition to False, Reason to "SchedulingGatesPresent"
		// and Message to "Scheduling gates present"

		// Collect a list of scheduling gate names
		var schedulingGateNames []string
		for _, gate := range instance.Spec.Controller.SchedulingGates {
			schedulingGateNames = append(schedulingGateNames, gate.Name)
		}

		if _, err := controllerutil.CreateOrPatch(ctx, cl.GetClient(), &instance, func() error {
			apimeta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
				Type:               computev1alpha.InstanceReady,
				Status:             metav1.ConditionFalse,
				Reason:             computev1alpha.InstanceReadyReasonSchedulingGatesPresent,
				Message:            fmt.Sprintf("Scheduling gates present: %s", strings.Join(schedulingGateNames, ", ")),
				ObservedGeneration: instance.Generation,
			})
			return nil
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed updating instance status: %w", err)
		}

		return ctrl.Result{}, nil
	}

	if condition := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha.InstanceProgrammed); condition.Status != metav1.ConditionTrue {
		logger.Info("instance is not programmed", "instance", instance.Name)
		message := "Instance has not been programmed"
		if condition.Status != metav1.ConditionUnknown {
			message = condition.Message
		}

		logger.Info("updating instance status", "instance", instance.Name)
		if _, err := controllerutil.CreateOrPatch(ctx, cl.GetClient(), &instance, func() error {
			apimeta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
				Type:               computev1alpha.InstanceReady,
				Status:             metav1.ConditionFalse,
				Reason:             computev1alpha.InstanceProgrammedReasonNotProgrammed,
				Message:            message,
				ObservedGeneration: instance.Generation,
			})
			return nil
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed updating instance status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	logger.Info("instance is programmed", "instance", instance.Name)

	if _, err := controllerutil.CreateOrPatch(ctx, cl.GetClient(), &instance, func() error {
		apimeta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               computev1alpha.InstanceReady,
			Status:             metav1.ConditionTrue,
			Reason:             computev1alpha.InstanceProgrammedReasonProgrammed,
			Message:            "Instance is ready",
			ObservedGeneration: instance.Generation,
		})
		return nil
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed updating instance status: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstanceReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr
	return mcbuilder.ControllerManagedBy(mgr).
		For(&computev1alpha.Instance{}, mcbuilder.WithEngageWithLocalCluster(false)).
		Complete(r)
}
