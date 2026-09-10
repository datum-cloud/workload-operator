// SPDX-License-Identifier: AGPL-3.0-only

// Package quotaview reads a project's compute quota and renders it as display
// rows.
//
// It is deliberately free of cobra and of the datumctl plugin runtime: the same
// numbers are read by `datumctl compute quota` and by the MCP server's
// compute_quota_get tool, and there is one implementation so the two can never
// disagree about what a project has left.
package quotaview

import (
	"context"
	"sort"
	"strings"

	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ComputeResourceTypePrefix selects the resource types compute owns.
	ComputeResourceTypePrefix = "compute.datumapis.com"

	// quotaNamespace is where a project's quota objects live inside its own
	// control plane.
	quotaNamespace = "milo-system"

	// consumerKindLabel and consumerKindProject select the quota held by the
	// project itself, rather than by anything nested under it.
	consumerKindLabel   = "quota.miloapis.com/consumer-kind"
	consumerKindProject = "Project"
)

// QuotaRow holds display-ready quota data for one resource type.
type QuotaRow struct {
	ResourceType string `json:"resourceType"`
	DisplayName  string `json:"displayName"`
	Unit         string `json:"unit"`
	Limit        int64  `json:"limit"`
	Used         int64  `json:"used"`
	Available    int64  `json:"available"`
}

// QuotaMeta overrides display metadata for a resource type. When provided,
// DisplayName, Unit, and Divisor take precedence over registered values.
type QuotaMeta struct {
	DisplayName string
	Unit        string
	// Divisor converts the stored integer value to display units (e.g. 1000 for
	// millicores → vCPUs). Zero is treated as 1.
	Divisor int64
	// Order controls the position of this row in the returned slice (ascending).
	// Rows without a meta entry sort after all meta rows, alphabetically.
	Order int
}

// ComputeOrderedTypes is the order compute's resource types are displayed in:
// the things a person counts first, first.
var ComputeOrderedTypes = []string{
	"compute.datumapis.com/workloads",
	"compute.datumapis.com/instances",
	"compute.datumapis.com/vcpus",
	"compute.datumapis.com/memory",
}

// ComputeMeta supplies display overrides for compute's resource types. The
// live registrations declare a display unit of "1", which tells a reader
// nothing, so the units are named here instead. vCPUs are stored in
// millicores, hence the divisor.
var ComputeMeta = map[string]QuotaMeta{
	"compute.datumapis.com/workloads": {DisplayName: "Workloads", Unit: "workloads", Divisor: 1},
	"compute.datumapis.com/instances": {DisplayName: "Instances", Unit: "instances", Divisor: 1},
	"compute.datumapis.com/vcpus":     {DisplayName: "vCPUs", Unit: "vCPUs", Divisor: 1000},
	"compute.datumapis.com/memory":    {DisplayName: "Memory", Unit: "MiB", Divisor: 1},
}

// ListServiceQuota returns quota rows for the project's quota whose resource
// type begins with resourceTypePrefix (e.g. "compute.datumapis.com").
// projectClient must target the project; platformClient must target the
// platform API server, and supplies display metadata when meta carries no
// override.
//
// platformClient may be nil. A caller that reads only as the person who asked
// holds no platform credential of its own, and display metadata is not worth
// failing a read over: without it, units fall back to the generic "units".
//
// meta may be nil. When an entry exists for a resource type, its DisplayName,
// Unit, and Divisor are used; otherwise the registered display unit is used and
// the divisor defaults to 1.
func ListServiceQuota(
	ctx context.Context,
	projectClient, platformClient client.Client,
	resourceTypePrefix string,
	meta map[string]QuotaMeta,
	orderedTypes []string, // explicit display order; types not in this list follow alphabetically
) ([]QuotaRow, error) {
	var bucketList quotav1alpha1.AllowanceBucketList
	if err := projectClient.List(ctx, &bucketList,
		client.InNamespace(quotaNamespace),
		client.MatchingLabels{consumerKindLabel: consumerKindProject},
	); err != nil {
		return nil, err
	}

	// Index by resource type, filtering to the requested prefix.
	bucketByType := make(map[string]*quotav1alpha1.AllowanceBucket)
	for i := range bucketList.Items {
		b := &bucketList.Items[i]
		if strings.HasPrefix(b.Spec.ResourceType, resourceTypePrefix) {
			bucketByType[b.Spec.ResourceType] = b
		}
	}

	if len(bucketByType) == 0 {
		return nil, nil
	}

	// Display metadata fallback, best effort: a caller with no platform
	// credential still gets numbers.
	rrByType := make(map[string]*quotav1alpha1.ResourceRegistration)
	if platformClient != nil {
		var rrList quotav1alpha1.ResourceRegistrationList
		if err := platformClient.List(ctx, &rrList); err == nil {
			for i := range rrList.Items {
				rr := &rrList.Items[i]
				if strings.HasPrefix(rr.Spec.ResourceType, resourceTypePrefix) {
					rrByType[rr.Spec.ResourceType] = rr
				}
			}
		}
	}

	rows := make([]QuotaRow, 0, len(bucketByType))
	seen := make(map[string]bool, len(bucketByType))

	appendRow := func(rt string, b *quotav1alpha1.AllowanceBucket) {
		if seen[rt] {
			return
		}
		seen[rt] = true

		displayName := resourceTypeSuffix(rt)
		unit := "units"
		var divisor int64 = 1

		if m, ok := meta[rt]; ok {
			if m.DisplayName != "" {
				displayName = m.DisplayName
			}
			if m.Unit != "" {
				unit = m.Unit
			}
			if m.Divisor > 1 {
				divisor = m.Divisor
			}
		} else if rr, ok := rrByType[rt]; ok && rr.Spec.DisplayUnit != "" && rr.Spec.DisplayUnit != "1" {
			unit = rr.Spec.DisplayUnit
		}

		rows = append(rows, QuotaRow{
			ResourceType: rt,
			DisplayName:  displayName,
			Unit:         unit,
			Limit:        b.Status.Limit / divisor,
			Used:         b.Status.Allocated / divisor,
			Available:    b.Status.Available / divisor,
		})
	}

	for _, rt := range orderedTypes {
		if b, ok := bucketByType[rt]; ok {
			appendRow(rt, b)
		}
	}

	// Anything the caller did not order sorts alphabetically behind it, so the
	// output stays reproducible when a new resource type appears.
	remaining := make([]string, 0, len(bucketByType))
	for rt := range bucketByType {
		if !seen[rt] {
			remaining = append(remaining, rt)
		}
	}
	sort.Strings(remaining)
	for _, rt := range remaining {
		appendRow(rt, bucketByType[rt])
	}

	return rows, nil
}

// ListComputeQuota is ListServiceQuota with compute's own prefix, display
// metadata and order applied.
func ListComputeQuota(
	ctx context.Context, projectClient, platformClient client.Client,
) ([]QuotaRow, error) {
	return ListServiceQuota(
		ctx, projectClient, platformClient,
		ComputeResourceTypePrefix, ComputeMeta, ComputeOrderedTypes,
	)
}

// resourceTypeSuffix derives a human-readable name from the last segment of a
// resource type string (e.g. "compute.datumapis.com/vcpus" → "vcpus").
func resourceTypeSuffix(resourceType string) string {
	if idx := strings.LastIndex(resourceType, "/"); idx >= 0 {
		return resourceType[idx+1:]
	}
	return resourceType
}
