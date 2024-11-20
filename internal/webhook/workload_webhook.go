package webhook

import (
	"context"

	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	computev1alpha "go.datum.net/workload-operator/api/v1alpha"
	"go.datum.net/workload-operator/internal/validation"
)

// SetupWorkloadWebhookWithManager will setup the manager to manage workload
// webhooks
func SetupWorkloadWebhookWithManager(mgr ctrl.Manager) error {
	client := mgr.GetClient()

	webhook := &workloadWebhook{
		Client: client,
		logger: mgr.GetLogger(),
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&computev1alpha.Workload{}).
		WithDefaulter(webhook).
		WithValidator(webhook).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-compute-datumapis-com-v1alpha-workload,mutating=true,failurePolicy=fail,sideEffects=None,groups=compute.datumapis.com,resources=workloads,verbs=create;update,versions=v1alpha,name=mworkload.kb.io,admissionReviewVersions=v1

type workloadWebhook struct {
	client.Client
	logger logr.Logger
}

var _ admission.CustomDefaulter = &workloadWebhook{}
var _ admission.CustomValidator = &workloadWebhook{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *workloadWebhook) Default(ctx context.Context, obj runtime.Object) error {
	workload, ok := obj.(*computev1alpha.Workload)
	if !ok {
		return fmt.Errorf("unexpected type %T", obj)
	}
	_ = workload

	// TODO(jreese) review and test gateway defaulting / logic
	if gw := workload.Spec.Gateway; gw != nil {
		for i, tcpRoute := range gw.TCPRoutes {
			for j := range tcpRoute.ParentRefs {
				workload.Spec.Gateway.TCPRoutes[i].ParentRefs[j].Name = "workload-gateway"
			}

			for j := range tcpRoute.Rules {
				for k := range tcpRoute.Rules[j].BackendRefs {
					// TODO(jreese) think about this Kind more
					kind := gatewayv1.Kind("NamedPort")
					workload.Spec.Gateway.TCPRoutes[i].Rules[j].
						BackendRefs[k].Kind = &kind
				}
			}
		}
	}

	// TODO(user): fill in your defaulting logic.
	return nil
}

// +kubebuilder:webhook:path=/validate-compute-datumapis-com-v1alpha-workload,mutating=false,failurePolicy=fail,sideEffects=None,groups=compute.datumapis.com,resources=workloads,verbs=create;update,versions=v1alpha,name=vworkload.kb.io,admissionReviewVersions=v1

func (r *workloadWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	workload, ok := obj.(*computev1alpha.Workload)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, err
	}

	opts := validation.WorkloadValidationOptions{
		Context:          ctx,
		Client:           r.Client,
		AdmissionRequest: req,
		Workload:         workload,
	}

	if errs := validation.ValidateWorkloadCreate(workload, opts); len(errs) > 0 {
		return nil, errors.NewInvalid(obj.GetObjectKind().GroupVersionKind().GroupKind(), workload.Name, errs)
	}

	return nil, nil
}

func (r *workloadWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldworkload, ok := oldObj.(*computev1alpha.Workload)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", oldObj)
	}

	_ = oldworkload

	newworkload, ok := newObj.(*computev1alpha.Workload)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", newObj)
	}

	_ = newworkload

	// TODO(user): fill in your validation logic upon object update.
	return nil, nil
}

func (r *workloadWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	workload, ok := obj.(*computev1alpha.Workload)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	_ = workload

	// TODO(user): fill in your validation logic upon object deletion.
	return nil, nil
}