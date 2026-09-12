package util

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.datum.net/compute/internal/quotaview"
)

// The quota reading lives in internal/quotaview so the CLI and the MCP server
// share one implementation — the numbers a person is shown and the numbers an
// assistant reports must not be able to drift apart. These aliases keep the
// existing call sites in this package's consumers working.

// QuotaRow holds display-ready quota data for one resource type.
type QuotaRow = quotaview.QuotaRow

// QuotaMeta overrides display metadata for a resource type.
type QuotaMeta = quotaview.QuotaMeta

// ListServiceQuota returns quota rows for the project's quota whose resource
// type begins with resourceTypePrefix. See quotaview.ListServiceQuota.
func ListServiceQuota(
	ctx context.Context,
	projectClient, platformClient client.Client,
	resourceTypePrefix string,
	meta map[string]QuotaMeta,
	orderedTypes []string,
) ([]QuotaRow, error) {
	return quotaview.ListServiceQuota(ctx, projectClient, platformClient, resourceTypePrefix, meta, orderedTypes)
}
