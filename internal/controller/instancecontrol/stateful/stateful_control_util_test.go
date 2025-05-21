package stateful

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.datum.net/workload-operator/internal/controller/instancecontrol"
)

func TestGetInstanceOrdinal(t *testing.T) {
	tests := []struct {
		name       string
		objectName string
		want       int
	}{
		{
			name:       "instance with ordinal 0",
			objectName: "my-instance-0",
			want:       0,
		},
		{
			name:       "instance with ordinal 1",
			objectName: "my-instance-1",
			want:       1,
		},
		{
			name:       "instance with unexpected suffix",
			objectName: "my-instance-foo",
			want:       -1,
		},
		{
			name:       "instance with no dash in name",
			objectName: "myinstance",
			want:       -1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := getInstanceOrdinal(test.objectName)
			if got != test.want {
				t.Errorf("getInstanceOrdinal(%q) = %d, want %d", test.objectName, got, test.want)
			}
		})
	}
}

func TestDescendingOrdinal(t *testing.T) {
	actions := make([]instancecontrol.Action, 0, 4)

	perm := rand.Perm(4)
	for i := range perm {
		actions = append(actions, instancecontrol.NewWaitAction(
			fmt.Sprintf("my-instance-%d", perm[i]),
		))
	}

	slices.SortFunc(actions, descendingOrdinal)

	assert.Equal(t, actions[0].ObjectName, "my-instance-3")
	assert.Equal(t, actions[1].ObjectName, "my-instance-2")
	assert.Equal(t, actions[2].ObjectName, "my-instance-1")
	assert.Equal(t, actions[3].ObjectName, "my-instance-0")
}
