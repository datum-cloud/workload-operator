// SPDX-License-Identifier: AGPL-3.0-only

package workloadspec

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
			lines = append(lines, fmt.Sprintf("  + new placement %q: %s", np.Name, placementWhere(np)))
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

// placementWhere describes where a placement runs, in whichever of the two
// forms it was written: a fixed list of locations, or a selector over their
// topology. A selector is shown as the selector, not as the locations it
// happens to match today, because that is what is being added.
func placementWhere(p computev1alpha.WorkloadPlacement) string {
	if p.LocationSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(p.LocationSelector)
		if err != nil {
			return "locationSelector=<invalid>"
		}
		return "locationSelector=" + selector.String()
	}

	names := make([]string, 0, len(p.Locations))
	for _, location := range p.Locations {
		names = append(names, location.Name)
	}
	return fmt.Sprintf("locations=[%s]", strings.Join(names, ", "))
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
