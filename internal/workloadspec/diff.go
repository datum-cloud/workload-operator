// SPDX-License-Identifier: AGPL-3.0-only

package workloadspec

import (
	"fmt"
	"strings"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

// Diff summarizes what applying desired would change about existing, as lines
// meant to be shown to a person before they confirm an apply. An empty result
// means nothing Diff reports on changed — it covers the image and the
// placements, not the whole spec.
//
// Lines are ordered by the desired manifest, then by the existing one, so the
// same pair of manifests always produces the same output.
func Diff(existing, desired *computev1alpha.Workload) []string {
	var lines []string

	oldImage := imageOf(existing)
	newImage := imageOf(desired)
	if oldImage != newImage {
		lines = append(lines, fmt.Sprintf("  image: %s → %s", oldImage, newImage))
	}

	oldPlacements := placementsByName(existing)

	seen := make(map[string]struct{}, len(oldPlacements))
	for _, np := range placementsOf(desired) {
		seen[np.Name] = struct{}{}

		op, ok := oldPlacements[np.Name]
		if !ok {
			lines = append(lines, fmt.Sprintf("  + new placement %q: cities=[%s]",
				np.Name, strings.Join(np.CityCodes, ", ")))
			continue
		}

		if op.ScaleSettings.MinReplicas != np.ScaleSettings.MinReplicas {
			lines = append(lines, fmt.Sprintf("  placement %q min replicas: %d → %d",
				np.Name, op.ScaleSettings.MinReplicas, np.ScaleSettings.MinReplicas))
		}
	}

	for _, op := range placementsOf(existing) {
		if _, ok := seen[op.Name]; !ok {
			lines = append(lines, fmt.Sprintf("  - removed placement %q", op.Name))
		}
	}

	return lines
}

// imageOf returns the first container image found in a workload, or the empty
// string when there is none (a nil workload, or a VM runtime).
func imageOf(w *computev1alpha.Workload) string {
	if w == nil {
		return ""
	}
	sandbox := w.Spec.Template.Spec.Runtime.Sandbox
	if sandbox != nil && len(sandbox.Containers) > 0 {
		return sandbox.Containers[0].Image
	}
	return ""
}

func placementsOf(w *computev1alpha.Workload) []computev1alpha.WorkloadPlacement {
	if w == nil {
		return nil
	}
	return w.Spec.Placements
}

func placementsByName(w *computev1alpha.Workload) map[string]computev1alpha.WorkloadPlacement {
	placements := placementsOf(w)
	byName := make(map[string]computev1alpha.WorkloadPlacement, len(placements))
	for _, p := range placements {
		byName[p.Name] = p
	}
	return byName
}
