// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.datum.net/compute/internal/cmd/compute/url"
	"go.datum.net/compute/internal/cmd/compute/util"
)

func removeCommand() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "remove <workload> <hostname>",
		Short: "Detach a custom hostname from a workload",
		Long: "Detach a custom hostname from a workload's URL.\n\n" +
			"The workload keeps serving on its Datum-managed hostname, which is permanent\n" +
			"and cannot be removed.\n\n" +
			"The domain itself is left alone: it stays verified in this project and can be\n" +
			"attached to this or another workload without verifying it again.",
		Example: "  # Detach a hostname\n" +
			"  datumctl compute domains remove api api.example.com\n\n" +
			"  # Without the confirmation prompt\n" +
			"  datumctl compute domains remove api api.example.com --yes",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := util.NewClient(util.ProjectFromCmd(cmd))
			if err != nil {
				return err
			}
			return runRemove(cmd.Context(), cmd.OutOrStdout(), c, util.ProjectFromCmd(cmd), args[0], args[1], yes)
		},
		ValidArgsFunction: completeRemoveArgs,
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")

	return cmd
}

// runRemove detaches a hostname from a workload's URL.
//
// The Datum-managed hostname is refused: it is the workload's permanent
// address and the fallback a user checks when a custom domain is misbehaving.
// The check is made on the normalized form of both names, so pasting
// "https://A1B2C3D4.DatumProxy.net/" back out of a browser is refused just as
// plainly as typing the hostname exactly.
func runRemove(ctx context.Context, out io.Writer, c client.Client, project, workloadName, rawHostname string, yes bool) error {
	if ctx == nil {
		ctx = context.Background()
	}

	hostname := normalizeHostname(rawHostname)
	if hostname == "" {
		return fmt.Errorf("no hostname given — pass the hostname to detach, for example api.example.com")
	}

	info, err := urlFor(ctx, c, project, workloadName)
	if err != nil {
		return err
	}

	if managed(info, hostname) {
		return fmt.Errorf(
			"%s is the Datum-managed domain for workload %q and cannot be removed — it is permanent, and stops serving only when the workload stops declaring an HTTP port",
			hostname, workloadName,
		)
	}

	index := indexOf(info.Proxy.Spec.Hostnames, hostname)
	if index < 0 {
		return notAttached(info, workloadName, hostname)
	}

	// The stored spelling, not the user's: the confirmation and the summary
	// should show what is actually on the workload.
	attachedAs := string(info.Proxy.Spec.Hostnames[index])

	ok, err := confirm(out, fmt.Sprintf(
		"Remove %s from workload %q? It will stop serving on that hostname. (y/N): ", attachedAs, workloadName), yes)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "Aborted.")
		return nil
	}

	proxy := info.Proxy.DeepCopy()
	proxy.Spec.Hostnames = append(proxy.Spec.Hostnames[:index], proxy.Spec.Hostnames[index+1:]...)
	if err := c.Update(ctx, proxy); err != nil {
		return fmt.Errorf("detaching %s from workload %q: %w", attachedAs, workloadName, err)
	}

	fmt.Fprintf(out, "  %s removed from workload %q.\n", attachedAs, workloadName)
	if info.CanonicalHostname != "" {
		fmt.Fprintf(out, "  Still serving on https://%s\n", info.CanonicalHostname)
	}
	fmt.Fprintln(out, "\n  The domain stays verified in this project. Attach it again with:")
	fmt.Fprintf(out, "    datumctl compute domains add %s %s\n", workloadName, attachedAs)

	return nil
}

// managed reports whether a hostname is the one the platform assigned. Both the
// status field and the parsed hostname list are checked, so the refusal does
// not depend on which of the two the server filled in.
func managed(info *url.Info, hostname string) bool {
	if sameHostname(info.CanonicalHostname, hostname) {
		return true
	}
	for _, h := range info.Hostnames {
		if h.Managed && sameHostname(h.Hostname, hostname) {
			return true
		}
	}
	return false
}

// indexOf finds a hostname in the proxy's declared list, comparing normalized
// forms so a trailing dot or a different case still matches what is stored.
func indexOf(hostnames []gatewayv1.Hostname, hostname string) int {
	for i, h := range hostnames {
		if sameHostname(string(h), hostname) {
			return i
		}
	}
	return -1
}

// notAttached explains that there is nothing to remove, and lists what could
// be removed instead.
func notAttached(info *url.Info, workloadName, hostname string) error {
	if len(info.CustomHostnames) == 0 {
		return fmt.Errorf("workload %q has no custom domains — it serves on its Datum-managed domain only", workloadName)
	}
	return fmt.Errorf("%s is not attached to workload %q — it serves on: %s",
		hostname, workloadName, strings.Join(info.CustomHostnames, ", "))
}

// confirm asks before doing something that stops traffic. Without a terminal
// to ask on it refuses rather than assuming: a script that meant to remove a
// domain can say so with --yes.
func confirm(out io.Writer, prompt string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, fmt.Errorf("refusing to remove a domain without confirmation — re-run with --yes")
	}

	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	line = strings.TrimSpace(line)
	return line == "y" || line == "Y", nil
}

// completeRemoveArgs completes the workload first and then the hostnames that
// workload actually serves, minus the Datum-managed one, which cannot be
// removed and so should never be offered.
func completeRemoveArgs(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return util.CompleteWorkloadNames(cmd, args, "")
	}
	if len(args) > 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	c, err := util.NewClient(util.ProjectFromCmd(cmd))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	info, err := url.ForWorkload(context.Background(), c, args[0])
	if err != nil || info == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return info.CustomHostnames, cobra.ShellCompDirectiveNoFileComp
}
