package stateful

import (
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"

	"go.datum.net/workload-operator/api/v1alpha"
	"go.datum.net/workload-operator/internal/controller/instancecontrol"
)

func needsUpdate(instance *v1alpha.Instance, deployment *v1alpha.WorkloadDeployment) bool {
	labels := instance.Labels
	if labels != nil {
		instance = instance.DeepCopy()
		delete(instance.Labels, v1alpha.InstanceIndexLabel)
	}

	return !equality.Semantic.DeepEqual(instance.Annotations, deployment.Spec.Template.Annotations) ||
		!equality.Semantic.DeepEqual(instance.Labels, deployment.Spec.Template.Labels) ||
		!equality.Semantic.DeepEqual(instance.Spec, deployment.Spec.Template.Spec)
}

// getInstanceOrdinal returns the ordinal of the instance, or -1 if the instance
// does not have an ordinal.
func getInstanceOrdinal(name string) int {
	lastDash := strings.LastIndex(name, "-")
	if lastDash == -1 {
		return -1
	}

	ordinal := -1
	if i, err := strconv.Atoi(name[lastDash+1:]); err == nil {
		ordinal = i
	}

	return ordinal
}

func ascendingOrdinal(a, b instancecontrol.Action) int {
	if getInstanceOrdinal(a.ObjectName) < getInstanceOrdinal(b.ObjectName) {
		return -1
	} else {
		return 1
	}
}

func descendingOrdinal(a, b instancecontrol.Action) int {
	if getInstanceOrdinal(a.ObjectName) > getInstanceOrdinal(b.ObjectName) {
		return -1
	} else {
		return 1
	}
}
