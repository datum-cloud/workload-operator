// SPDX-License-Identifier: AGPL-3.0-only

// Package domains implements `datumctl compute domains`: the project-wide list
// of every hostname a workload answers on, the per-URL detail view a developer
// reaches for when a URL is not working, and the two commands that attach and
// detach a custom hostname.
//
// Everything here reads and writes through the url package, which owns how a
// workload becomes a URL. Nothing here names the machinery: the vocabulary is
// "domain", "certificate", "backends".
package domains

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/internal/cmd/compute/url"
	"go.datum.net/compute/internal/cmd/compute/util"
)

// managedMarker flags the hostname the platform assigned. It is the one row in
// the list a user cannot act on, so it says so on the row rather than only in
// the error they would get if they tried.
const managedMarker = "(Datum-managed)"

// dash is what an unreported value renders as, so a column is never blank.
const dash = "—"

// Command returns the "domains" command group.
func Command() *cobra.Command {
	var output string
	var noHeaders bool

	cmd := &cobra.Command{
		Use:   "domains [workload]",
		Short: "List the domains in a project, or show one workload's URL in detail",
		Long: "List every domain in the project, or pass a workload to see its URL in detail.\n\n" +
			"The detail view is the one to reach for when a URL is not answering: it shows\n" +
			"every hostname the workload serves, its certificate, and how many backends are\n" +
			"healthy in each city.\n\n" +
			"Every workload that declares an HTTP port gets a permanent Datum-managed domain.\n" +
			"It always appears in this list and cannot be removed.",
		Example: "  # Every domain in the project\n" +
			"  datumctl compute domains\n\n" +
			"  # One workload's URL, in detail\n" +
			"  datumctl compute domains api\n\n" +
			"  # Attach a custom hostname\n" +
			"  datumctl compute domains add api api.example.com\n\n" +
			"  # Machine-readable output\n" +
			"  datumctl compute domains -o json",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := util.NewClient(util.ProjectFromCmd(cmd))
			if err != nil {
				return err
			}
			format := util.OutputFormat(output)
			if len(args) == 1 {
				return runDetail(cmd.Context(), cmd.OutOrStdout(), c, util.ProjectFromCmd(cmd), args[0], format)
			}
			return runList(cmd.Context(), cmd.OutOrStdout(), c, util.ProjectFromCmd(cmd), format, noHeaders)
		},
		ValidArgsFunction: util.CompleteWorkloadNames,
	}

	cmd.Flags().StringVarP(&output, "output", "o", "table", "Output format: table, json, yaml")
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "Omit the table header row (table only)")
	_ = cmd.RegisterFlagCompletionFunc("output", util.CompleteOutputFormats("table", "json", "yaml"))

	cmd.AddCommand(addCommand(), removeCommand())

	return cmd
}

// -----------------------------------------------------------------------
// domains — the project-wide list
// -----------------------------------------------------------------------

// listRow is one hostname in the project-wide list. It is also the JSON and
// YAML shape, so `-o json | jq -r '.[].domain'` is a list of hostnames and
// nothing more needs to be parsed out of a table.
type listRow struct {
	// Workload is the workload serving this domain.
	Workload string `json:"workload"`
	// Domain is the bare hostname; URL is the same thing as an https:// URL.
	Domain string `json:"domain"`
	URL    string `json:"url"`
	// Managed is true for the permanent hostname the platform assigned, which
	// cannot be removed.
	Managed bool `json:"managed"`
	// Status is "active", or whatever the server says is blocking it.
	Status string `json:"status"`
	// Certificate is "valid", or the server's reason, or empty when the
	// platform has not reported on one.
	Certificate string `json:"certificate,omitempty"`
	// Detail is the server's message for the first blocking condition.
	Detail string `json:"detail,omitempty"`
}

// runList renders every domain in the project. It costs two List calls in
// total, however many workloads there are, because url.ForAll does.
func runList(ctx context.Context, out io.Writer, c client.Client, project string, format util.OutputFormat, noHeaders bool) error {
	if ctx == nil {
		ctx = context.Background()
	}

	infos, err := url.ForAll(ctx, c)
	if err != nil {
		return err
	}
	rows := listRows(infos)

	switch format {
	case util.OutputJSON:
		return util.PrintJSON(out, rows)
	case util.OutputYAML:
		return util.PrintYAML(out, rows)
	}

	renderList(out, rows, project, noHeaders)
	return nil
}

// listRows flattens the per-workload URL info into one row per hostname,
// ordered by workload so the list is stable between runs. Within a workload
// the custom hostnames come first and the managed one last, which is the order
// url.Info already carries: the name the user chose is the one they are
// looking for.
func listRows(infos map[string]*url.Info) []listRow {
	names := make([]string, 0, len(infos))
	for name := range infos {
		names = append(names, name)
	}
	sort.Strings(names)

	var rows []listRow
	for _, name := range names {
		for _, h := range infos[name].Hostnames {
			rows = append(rows, listRow{
				Workload:    name,
				Domain:      h.Hostname,
				URL:         h.URL,
				Managed:     h.Managed,
				Status:      h.Status,
				Certificate: h.Certificate,
				Detail:      h.Detail,
			})
		}
	}
	return rows
}

// renderList writes the table. A project with no domains gets a sentence that
// says how to get one rather than an empty table.
func renderList(out io.Writer, rows []listRow, project string, noHeaders bool) {
	fmt.Fprintln(out)

	if len(rows) == 0 {
		fmt.Fprintf(out, "No domains in project %s.\n\n", project)
		fmt.Fprintln(out, "  A workload gets a permanent URL when it declares an HTTP port:")
		fmt.Fprintln(out, "    datumctl compute deploy <name> --image=<image> --http-port 8080")
		return
	}

	tw := util.NewTabWriter(out)
	if !noHeaders {
		fmt.Fprintln(tw, "  WORKLOAD\tDOMAIN\tSTATUS\tCERTIFICATE")
	}
	for _, r := range rows {
		certificate := r.Certificate
		if certificate == "" {
			certificate = dash
		}
		// The marker rides on the end of the last column rather than forming a
		// column of its own: it is an annotation on the row, and a column of
		// mostly-blank cells would pad every other row out to its width.
		if r.Managed {
			certificate += "  " + managedMarker
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.Workload, r.Domain, r.Status, certificate)
	}
	_ = tw.Flush()
}

// -----------------------------------------------------------------------
// domains <workload> — the per-URL detail view
// -----------------------------------------------------------------------

// runDetail renders one workload's URL: its hostnames, its certificate, and
// the per-city backend table that makes a multi-city deployment legible.
func runDetail(ctx context.Context, out io.Writer, c client.Client, project, workloadName string, format util.OutputFormat) error {
	if ctx == nil {
		ctx = context.Background()
	}

	info, err := url.ForWorkload(ctx, c, workloadName)
	if err != nil {
		return err
	}
	if info == nil {
		return notPublished(ctx, c, project, workloadName)
	}

	switch format {
	case util.OutputJSON:
		// JSON is the scripting view: the summary a caller wants to read a URL
		// or a health count out of.
		return util.PrintJSON(out, info)
	case util.OutputYAML:
		// YAML is the escape hatch the spec promises — the real platform
		// objects, for when the summary is not enough to debug with.
		return util.PrintYAML(out, info.Objects())
	}

	fmt.Fprintln(out)
	url.RenderDetail(out, info)
	return nil
}

// notPublished explains that a workload has no URL. A workload that does not
// exist and a workload that exists but never declared an HTTP port are
// different problems with different fixes, so they get different errors.
func notPublished(ctx context.Context, c client.Client, project, workloadName string) error {
	var workload computev1alpha.Workload
	err := c.Get(ctx, types.NamespacedName{Namespace: util.ResourceNamespace, Name: workloadName}, &workload)
	if k8serrors.IsNotFound(err) {
		return fmt.Errorf("workload %q not found in project %s — run 'datumctl compute workloads' to see what is deployed", workloadName, project)
	}
	return fmt.Errorf(
		"workload %q has no URL — it does not declare an HTTP port.\nTo publish it:  datumctl compute deploy %s --http-port <port>",
		workloadName, workloadName,
	)
}

// urlFor loads a workload's URL for a command that is about to change it,
// turning "no URL" into the error that says how to get one. Both add and
// remove need exactly this.
func urlFor(ctx context.Context, c client.Client, project, workloadName string) (*url.Info, error) {
	info, err := url.ForWorkload(ctx, c, workloadName)
	if err != nil {
		return nil, err
	}
	if info == nil || info.Proxy == nil {
		return nil, notPublished(ctx, c, project, workloadName)
	}
	return info, nil
}
