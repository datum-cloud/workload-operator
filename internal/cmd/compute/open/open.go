// SPDX-License-Identifier: AGPL-3.0-only

// Package open implements `datumctl compute open`: the one-word path from a
// workload name to the URL in a browser.
package open

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/internal/cmd/compute/url"
	"go.datum.net/compute/internal/cmd/compute/util"
)

// Command returns the `datumctl compute open` command.
func Command() *cobra.Command {
	var urlOnly bool

	cmd := &cobra.Command{
		Use:   "open <workload-name>",
		Short: "Open a workload's URL in the default browser",
		Long: "Open a workload's URL in the default browser.\n\n" +
			"With --url the URL is printed and nothing is opened, so it can be piped\n" +
			"into another command.",
		Example: "  # Open the URL for the \"api\" workload\n" +
			"  datumctl compute open api\n\n" +
			"  # Print the URL instead of opening it\n" +
			"  datumctl compute open api --url\n\n" +
			"  # Use it in a script\n" +
			"  curl -sS \"$(datumctl compute open api --url)/healthz\"",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := util.NewClient(util.ProjectFromCmd(cmd))
			if err != nil {
				return err
			}
			return run(cmd.Context(), cmd.OutOrStdout(), c, args[0], urlOnly, openBrowser)
		},
		ValidArgsFunction: util.CompleteWorkloadNames,
	}

	cmd.Flags().BoolVar(&urlOnly, "url", false, "Print the URL instead of opening it")

	return cmd
}

// run resolves the workload's URL and either prints it or opens it.
//
// Two failures a developer hits here are different problems with different
// fixes, so they get different errors: a workload that does not exist, and a
// workload that exists but has never been published.
func run(ctx context.Context, out io.Writer, c client.Client, workloadName string, urlOnly bool, open opener) error {
	if ctx == nil {
		ctx = context.Background()
	}

	var workload computev1alpha.Workload
	if err := c.Get(ctx, types.NamespacedName{Namespace: util.ResourceNamespace, Name: workloadName}, &workload); err != nil {
		if k8serrors.IsNotFound(err) {
			return fmt.Errorf("workload %q not found — run 'datumctl compute workloads' to see what is deployed", workloadName)
		}
		return fmt.Errorf("getting workload: %w", err)
	}

	info, err := url.ForWorkload(ctx, c, workloadName)
	if err != nil {
		return err
	}
	if info == nil {
		return fmt.Errorf(
			"workload %q has no URL — it does not declare an HTTP port.\nTo publish it:  datumctl compute deploy %s --http-port <port>",
			workloadName, workloadName,
		)
	}
	if info.URL == "" {
		return fmt.Errorf("workload %q does not have a URL yet — the platform is still assigning one. Try again in a moment", workloadName)
	}

	// Validate before anything else touches the URL, so the bare --url form is
	// held to the same standard as the one handed to a browser.
	if err := validateURL(info.URL); err != nil {
		return err
	}

	// The pipe-friendly form: the URL, a newline, nothing else.
	if urlOnly {
		fmt.Fprintln(out, info.URL)
		return nil
	}

	if !info.Live() {
		fmt.Fprintln(out, "The URL is not fully live yet — it may not respond immediately.")
	}
	fmt.Fprintf(out, "Opening %s\n", info.URL)

	return open(ctx, info.URL)
}
