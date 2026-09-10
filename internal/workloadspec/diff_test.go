// SPDX-License-Identifier: AGPL-3.0-only

package workloadspec

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

func TestDiff(t *testing.T) {
	base := func(tweaks ...func(*Input)) *computev1alpha.Workload {
		t.Helper()
		w, err := Render(validInput(tweaks...))
		if err != nil {
			t.Fatalf("Render() error: %v", err)
		}
		return w
	}

	cases := map[string]struct {
		existing *computev1alpha.Workload
		desired  *computev1alpha.Workload
		want     []string
	}{
		"no changes": {
			existing: base(),
			desired:  base(),
			want:     nil,
		},
		"image change": {
			existing: base(),
			desired:  base(func(in *Input) { in.Image = "ghcr.io/acme/api:2.0.0" }),
			want:     []string{"  image: ghcr.io/acme/api:1.4.2 → ghcr.io/acme/api:2.0.0"},
		},
		"replica change": {
			existing: base(),
			desired:  base(func(in *Input) { in.Placements[0].MinReplicas = 5 }),
			want:     []string{`  placement "us" min replicas: 2 → 5`},
		},
		"added and removed placements are reported in manifest order": {
			existing: base(),
			desired: base(func(in *Input) {
				in.Placements = []Placement{
					{Name: "eu", CityCodes: []string{"AMS", "FRA"}, MinReplicas: 1},
				}
			}),
			want: []string{
				`  + new placement "eu": cities=[AMS, FRA]`,
				`  - removed placement "us"`,
			},
		},
		"creating from nothing": {
			existing: nil,
			desired:  base(),
			want: []string{
				"  image:  → ghcr.io/acme/api:1.4.2",
				`  + new placement "us": cities=[DFW]`,
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if delta := cmp.Diff(tc.want, Diff(tc.existing, tc.desired)); delta != "" {
				t.Errorf("Diff() mismatch (-want +got):\n%s", delta)
			}
		})
	}
}
