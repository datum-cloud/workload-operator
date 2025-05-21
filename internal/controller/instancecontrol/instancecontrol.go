package instancecontrol

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.datum.net/workload-operator/api/v1alpha"
)

type Strategy interface {
	// GetActions returns a set of actions that should be taken in order to drive
	// the workload deployment to the desired state. Some actions may be informational,
	// such as providing context into pending actions when instance management
	// policies may require waiting for pod readiness.
	GetActions(
		ctx context.Context,
		scheme *runtime.Scheme,
		deployment *v1alpha.WorkloadDeployment,
		currentInstances []v1alpha.Instance,
	) ([]Action, error)
}

type ActionType int

const (
	ActionTypeCreate ActionType = iota
	ActionTypeUpdate
	ActionTypeDelete
	ActionTypeWait
)

type Action struct {
	ObjectName    string
	actionType    ActionType
	skipExecution bool
	fn            func(ctx context.Context, c client.Client) error
}

func (a Action) Execute(ctx context.Context, c client.Client) error {
	if a.skipExecution {
		return nil
	}

	return a.fn(ctx, c)
}

func (a *Action) SkipExecution() {
	a.skipExecution = true
}

func (a Action) IsSkipped() bool {
	return a.skipExecution
}

func (a Action) ActionType() ActionType {
	return a.actionType
}

func NewCreateAction(objectName string, f func(ctx context.Context, c client.Client) error) Action {
	return Action{
		ObjectName: objectName,
		actionType: ActionTypeCreate,
		fn:         f,
	}
}

func NewUpdateAction(objectName string, f func(ctx context.Context, c client.Client) error) Action {
	return Action{
		ObjectName: objectName,
		actionType: ActionTypeUpdate,
		fn:         f,
	}
}

func NewDeleteAction(objectName string, f func(ctx context.Context, c client.Client) error) Action {
	return Action{
		ObjectName: objectName,
		actionType: ActionTypeDelete,
		fn:         f,
	}
}

func NewWaitAction(objectName string) Action {
	return Action{
		ObjectName: objectName,
		actionType: ActionTypeWait,
		fn:         func(ctx context.Context, c client.Client) error { return nil },
	}
}
