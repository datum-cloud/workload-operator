// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.datum.net/compute/internal/cmd/compute/url"
	"go.datum.net/compute/internal/cmd/compute/util"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	// pollInterval matches the rollout watcher and the publish watcher, so
	// every wait in this CLI ticks at the same rate.
	pollInterval = 2 * time.Second

	// maxReadFailures is how many consecutive failed reads the wait rides out
	// before giving up and saying why, matching the publish watcher.
	maxReadFailures = 3

	// maxWait bounds the whole wait so no path hangs, however healthy the
	// polling looks.
	maxWait = 15 * time.Minute

	// blockingGrace is how long to wait before repeating what the server says
	// is holding a hostname up. Every hostname starts out unverified; saying so
	// in the first second is noise, not diagnosis.
	blockingGrace = 20 * time.Second

	// recordWindow is how long the platform gets to work out what it needs
	// from the user before this command stops waiting quietly and says so.
	//
	// A hostname is attached the instant the update lands, which is before the
	// platform has created the domain it belongs to or computed the record
	// that proves the user owns it. Printing the records block in that moment
	// would print it without the one record the whole flow turns on, and there
	// is no second chance: the block is printed once. So nothing is announced
	// until the records are known — and if they never are, that is said out
	// loud rather than waited out in silence.
	recordWindow = 60 * time.Second

	// stepWidth aligns the checkmarks in a fixed column.
	stepWidth = 24
)

// stepLabels turns the per-hostname condition types the platform publishes
// into the words a developer thinks in. This maps condition *types*, which are
// API constants; the reasons behind them are never interpreted, only echoed.
var stepLabels = map[string]string{
	networkingv1alpha.HostnameConditionVerified:            "Domain verified",
	networkingv1alpha.HostnameConditionAvailable:           "Hostname available",
	networkingv1alpha.HostnameConditionDNSRecordProgrammed: "DNS record programmed",
	networkingv1alpha.HostnameConditionCertificateReady:    "Certificate issued",
}

// stepOrder is the order the steps are reported in when several land at once.
// Conditions outside this list are shown after it, under their own names, so a
// check the platform adds tomorrow reaches the user without a CLI release.
var stepOrder = []string{
	networkingv1alpha.HostnameConditionVerified,
	networkingv1alpha.HostnameConditionAvailable,
	networkingv1alpha.HostnameConditionDNSRecordProgrammed,
	networkingv1alpha.HostnameConditionCertificateReady,
}

func addCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <workload> <hostname>",
		Short: "Attach a custom hostname to a workload",
		Long: "Attach a custom hostname to a workload's URL.\n\n" +
			"Datum prints the DNS records to create, then waits for the domain to verify,\n" +
			"for DNS to resolve, and for a certificate to be issued. Ctrl-C detaches — the\n" +
			"work continues, and 'datumctl compute domains' shows where it got to.\n\n" +
			"A workload may serve several custom hostnames. Attaching one never replaces\n" +
			"another: every hostname on a workload routes to that same workload, and the\n" +
			"workload keeps its Datum-managed hostname besides. To stop serving a hostname,\n" +
			"detach it with 'datumctl compute domains remove'.\n\n" +
			"Serving different content per hostname — path routing, host-based routing,\n" +
			"several backends behind one name — is manifest territory, not this command's.",
		Example: "  # Attach a hostname\n" +
			"  datumctl compute domains add api api.example.com\n\n" +
			"  # A second hostname, alongside the first\n" +
			"  datumctl compute domains add api www.example.com",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := util.NewClient(util.ProjectFromCmd(cmd))
			if err != nil {
				return err
			}
			// Ctrl-C detaches from the wait. It never cancels the attach: the
			// hostname is already on the proxy by the time we are waiting.
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()
			return runAdd(ctx, cmd.OutOrStdout(), c, util.ProjectFromCmd(cmd), args[0], args[1])
		},
		ValidArgsFunction: util.CompleteWorkloadNames,
	}

	return cmd
}

// runAdd appends a hostname to the workload's URL and waits for it to answer.
//
// Attaching the same hostname twice is not an error: the wait is the useful
// part of this command, and a user re-running it after a Ctrl-C wants to pick
// the wait back up, not to be told off.
func runAdd(ctx context.Context, out io.Writer, c client.Client, project, workloadName, rawHostname string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	hostname, err := validateHostname(rawHostname)
	if err != nil {
		return err
	}

	info, err := urlFor(ctx, c, project, workloadName)
	if err != nil {
		return err
	}

	if sameHostname(info.CanonicalHostname, hostname) {
		return fmt.Errorf("%s is already the Datum-managed hostname for workload %q", hostname, workloadName)
	}

	fmt.Fprintln(out)

	if attached(info, hostname) {
		fmt.Fprintf(out, "%s is already attached to workload %q.\n", hostname, workloadName)
	} else {
		proxy := info.Proxy.DeepCopy()
		proxy.Spec.Hostnames = append(proxy.Spec.Hostnames, gatewayv1.Hostname(hostname))
		if err := c.Update(ctx, proxy); err != nil {
			return fmt.Errorf("attaching %s to workload %q: %w", hostname, workloadName, err)
		}

		// Attaching is the one part of this command that is already done by
		// the time the waiting starts, so it is reported before the wait
		// rather than after it. A workload that now answers to more than one
		// custom hostname is told so plainly: nothing was replaced.
		fmt.Fprintf(out, "%s attached to workload %q.\n", hostname, workloadName)
		if total := len(info.CustomHostnames) + 1; total > 1 {
			fmt.Fprintf(out, "Workload %q now serves %d custom hostnames; all of them route to it.\n",
				workloadName, total)
		}
	}

	fmt.Fprintln(out)

	return waitForHostname(ctx, out, c, workloadName, hostname)
}

// attached reports whether a hostname is already on the URL.
func attached(info *url.Info, hostname string) bool {
	for _, h := range info.CustomHostnames {
		if sameHostname(h, hostname) {
			return true
		}
	}
	return false
}

// waitForHostname polls until the hostname is verified, resolving and covered
// by a certificate, printing each step as the platform reports it.
func waitForHostname(ctx context.Context, out io.Writer, c client.Client, workloadName, hostname string) error {
	w := &hostnameWatch{
		out:      out,
		hostname: hostname,
		started:  time.Now(),
		seen:     map[string]bool{},
	}

	// A read that keeps failing is not going to start working, and a wait that
	// hides it hangs forever; a single blip is ridden out. Same budget and same
	// reasoning as the publish watcher, which had this exact defect.
	failures := 0
	step := func() (bool, error) {
		done, err := w.check(ctx, c, workloadName)
		var re readError
		if errors.As(err, &re) {
			failures++
			if failures >= maxReadFailures {
				return false, fmt.Errorf("checking %s on workload %q: %w", hostname, workloadName, re.err)
			}
			return false, nil
		}
		failures = 0
		return done, err
	}

	done, err := step()
	if done || err != nil {
		return err
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Nothing bounds how long a developer waits on DNS they control, so the
	// deadline is the backstop that keeps the command from hanging rather than
	// a judgement about how long verification should take.
	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(out, "\nDetached. Verification continues in the background.\n")
			fmt.Fprintln(out, "  Check it with: datumctl compute domains")
			return nil

		case <-deadline.C:
			return fmt.Errorf("%s on workload %q was not verified within %s — check it with: datumctl compute domains",
				hostname, workloadName, maxWait)

		case <-ticker.C:
			done, err := step()
			if done || err != nil {
				return err
			}
		}
	}
}

// readError marks a failure to read the current state, as opposed to a failure
// that means waiting longer is pointless. Reads are retried; anything else ends
// the wait immediately.
type readError struct{ err error }

func (e readError) Error() string { return e.err.Error() }
func (e readError) Unwrap() error { return e.err }

// hostnameWatch prints the progress of one hostname exactly once per fact.
type hostnameWatch struct {
	out      io.Writer
	hostname string
	started  time.Time
	seen     map[string]bool
}

// check reads the current state and reports whether the hostname is live, and
// whether waiting any longer would be pointless.
//
// A read failure is reported as a readError rather than swallowed. One is
// nothing — the hostname was just written and the next tick is a better answer
// — but a control plane this command cannot read is not going to start
// answering, and a wait that hides that hangs forever. waitForHostname decides
// how many to ride out. The other error this returns is the one from announce:
// the platform never told the user what to create, so there is nothing left for
// a wait to do.
func (w *hostnameWatch) check(ctx context.Context, c client.Client, workloadName string) (bool, error) {
	info, err := url.ForWorkload(ctx, c, workloadName)
	if err != nil {
		return false, readError{err: err}
	}
	if info == nil {
		return false, nil
	}
	h := hostnameFrom(info, w.hostname)
	if h == nil {
		return false, nil
	}

	if hostnameLive(h) {
		w.steps(h)
		fmt.Fprintf(w.out, "\n  %s\n", h.URL)
		return true, nil
	}

	domain := domainFor(ctx, c, w.hostname)
	announced, err := w.announce(info, h, domain)
	if err != nil {
		return false, err
	}
	if !announced {
		// Checkmarks and blocking notes belong under the heading that has not
		// been printed yet. Nothing is said about a hostname before the user
		// has been told what the hostname is waiting for.
		return false, nil
	}

	w.steps(h)

	if time.Since(w.started) > blockingGrace {
		w.blocking(h, domain)
	}
	return false, nil
}

// announce prints the records the user has to create and the note that says
// Ctrl-C is safe, and reports whether it has done so yet.
//
// It waits for the platform to have worked out what it needs. A hostname is
// attached the moment the update lands, which is before the domain behind it
// exists and before the record that proves ownership has been computed; a
// records block printed then is a block with the TXT record missing, and the
// user is left waiting on DNS they were never shown. So until the records are
// known this prints nothing and asks to be called again — with one exception:
// once recordWindow has passed, whatever the platform did publish is printed
// along with a note about what it has not, and if that is nothing at all the
// wait ends with an error rather than continuing in silence.
//
// Nothing here is printed twice, and a record that only turns up later is
// still printed when it does.
func (w *hostnameWatch) announce(info *url.Info, h *url.Hostname, domain *networkingv1alpha.Domain) (bool, error) {
	records := recordsFor(info, h, domain)
	known := recordsKnown(info, h, domain)

	if !known && time.Since(w.started) < recordWindow {
		return false, nil
	}
	if !known && len(records) == 0 {
		return false, fmt.Errorf(
			"%s is attached to workload %q, but Datum has published nothing to create for it after %s — check it with: datumctl compute domains",
			w.hostname, info.WorkloadName, recordWindow)
	}

	name := w.hostname
	if domain != nil && domain.Spec.DomainName != "" {
		name = domain.Spec.DomainName
	}
	w.once("verifying", func() { fmt.Fprintf(w.out, "Verifying %s...\n", name) })

	w.records(records)

	if !known {
		w.once("incomplete", func() {
			fmt.Fprintf(w.out, "\n  Datum has not published a verification record for %s yet.\n", name)
			fmt.Fprintln(w.out, "  It will appear here if one is needed.")
		})
	}

	w.once("waiting", func() {
		fmt.Fprintln(w.out, "\nWaiting for DNS. Ctrl-C to detach — run 'datumctl compute domains' to check.")
		fmt.Fprintln(w.out)
	})
	return true, nil
}

// records prints the records the user has not been shown yet. Under normal
// conditions that is every record at once, in one block, because announce
// waits for them all; a record the platform only worked out afterwards gets a
// block of its own rather than going unmentioned.
func (w *hostnameWatch) records(records []record) {
	var unseen []record
	for _, r := range records {
		key := "record:" + r.Type + " " + r.Name + " " + r.Value
		if w.seen[key] {
			continue
		}
		w.seen[key] = true
		unseen = append(unseen, r)
	}
	if len(unseen) == 0 {
		return
	}

	fmt.Fprintln(w.out, "\n  Add these DNS records:")
	fmt.Fprintln(w.out)
	tw := util.NewTabWriter(w.out)
	fmt.Fprintln(tw, "    TYPE\tNAME\tVALUE")
	for _, r := range unseen {
		fmt.Fprintf(tw, "    %s\t%s\t%s\n", r.Type, r.Name, r.Value)
	}
	_ = tw.Flush()
}

// once runs f the first time it is asked to, and never again.
func (w *hostnameWatch) once(key string, f func()) {
	if w.seen[key] {
		return
	}
	w.seen[key] = true
	f()
}

// recordsKnown reports whether the platform has finished working out what it
// needs from the user for this hostname: whether ownership has to be proved
// and, if so, with which record, and what the hostname should be pointed at.
//
// It is deliberately about the platform having answered rather than about
// there being records to print: a domain already verified in this project
// needs no TXT record, and that is an answer.
func recordsKnown(info *url.Info, h *url.Hostname, domain *networkingv1alpha.Domain) bool {
	return verificationKnown(domain) && targetKnown(info, h)
}

// verificationKnown reports whether the platform has settled how ownership of
// the domain is proved. No Domain at all means it has not started: the
// platform creates one the first time it tries to program a hostname.
func verificationKnown(domain *networkingv1alpha.Domain) bool {
	if domain == nil {
		return false
	}
	if conditionTrue(domain.Status.Conditions, networkingv1alpha.DomainConditionVerified) {
		return true
	}
	v := domain.Status.Verification
	return v != nil && v.DNSRecord.Type != "" && v.DNSRecord.Name != "" && v.DNSRecord.Content != ""
}

// targetKnown reports whether the hostname can be pointed somewhere: either
// the platform has assigned the hostname to point at, or it has programmed the
// record itself and there is nothing for the user to point.
func targetKnown(info *url.Info, h *url.Hostname) bool {
	return info.CanonicalHostname != "" ||
		conditionTrue(h.Conditions, networkingv1alpha.HostnameConditionDNSRecordProgrammed)
}

// steps prints a checkmark for each check the platform reports as passed.
func (w *hostnameWatch) steps(h *url.Hostname) {
	for _, condType := range stepOrder {
		if c := util.FindCondition(h.Conditions, condType); c != nil && c.Status == metav1.ConditionTrue {
			w.step(condType)
		}
	}
	for _, c := range h.Conditions {
		if c.Status == metav1.ConditionTrue {
			w.step(c.Type)
		}
	}
}

// step prints one checkmark row, under the condition's own type when this CLI
// has no friendlier name for it.
func (w *hostnameWatch) step(condType string) {
	if w.seen["step:"+condType] {
		return
	}
	w.seen["step:"+condType] = true

	label, ok := stepLabels[condType]
	if !ok {
		label = condType
	}
	fmt.Fprintf(w.out, "  %-*s ✓\n", stepWidth, label)
}

// blocking echoes, verbatim and once each, whatever the server says is holding
// the hostname up — including the domain's own verification message, which is
// where "we looked for the TXT record and did not find it" shows up.
func (w *hostnameWatch) blocking(h *url.Hostname, domain *networkingv1alpha.Domain) {
	for _, c := range h.Conditions {
		if c.Status != metav1.ConditionTrue && c.Reason != "" {
			w.note(c.Reason, c.Message)
		}
	}
	if domain == nil {
		return
	}
	if c := util.FindCondition(domain.Status.Conditions, networkingv1alpha.DomainConditionVerified); c != nil &&
		c.Status != metav1.ConditionTrue && c.Reason != "" {
		w.note(c.Reason, c.Message)
	}
}

func (w *hostnameWatch) note(reason, message string) {
	text := url.HumanBlock(reason, message)
	if w.seen["note:"+text] {
		return
	}
	w.seen["note:"+text] = true
	fmt.Fprintf(w.out, "  %-*s %s\n", stepWidth, "", text)
}

// hostnameLive reports whether a hostname is ready to hand to the user: it is
// serving, and its certificate has been issued (or the platform has not
// reported a certificate check for it, in which case there is nothing to wait
// for).
func hostnameLive(h *url.Hostname) bool {
	if h == nil || !h.Active {
		return false
	}
	if c := util.FindCondition(h.Conditions, networkingv1alpha.HostnameConditionCertificateReady); c != nil {
		return c.Status == metav1.ConditionTrue
	}
	return true
}

// hostnameFrom finds one hostname's state in a workload's URL info.
func hostnameFrom(info *url.Info, hostname string) *url.Hostname {
	for i := range info.Hostnames {
		if sameHostname(info.Hostnames[i].Hostname, hostname) {
			return &info.Hostnames[i]
		}
	}
	return nil
}

// record is one DNS record the user has to create, exactly as the platform
// reports it. Nothing in this file synthesizes a record value: a record the
// server has not told us about is a record we do not print.
type record struct {
	Type  string
	Name  string
	Value string
}

// recordsFor returns the records still outstanding for a hostname: the TXT the
// platform wants for proof of ownership, and the CNAME that points the hostname
// at the platform's own.
//
// Each is dropped once the corresponding condition goes True — a domain that is
// already verified needs no TXT, and a hostname whose record the platform
// programmed itself needs no CNAME.
func recordsFor(info *url.Info, h *url.Hostname, domain *networkingv1alpha.Domain) []record {
	var records []record

	if domain != nil && !conditionTrue(domain.Status.Conditions, networkingv1alpha.DomainConditionVerified) {
		if v := domain.Status.Verification; v != nil {
			r := v.DNSRecord
			if r.Type != "" && r.Name != "" && r.Content != "" {
				records = append(records, record{Type: r.Type, Name: r.Name, Value: r.Content})
			}
		}
	}

	if info.CanonicalHostname != "" && !conditionTrue(h.Conditions, networkingv1alpha.HostnameConditionDNSRecordProgrammed) {
		records = append(records, record{Type: "CNAME", Name: h.Hostname, Value: info.CanonicalHostname})
	}

	return records
}

func conditionTrue(conditions []metav1.Condition, condType string) bool {
	c := util.FindCondition(conditions, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}

// domainFor returns the Domain resource covering a hostname.
//
// The platform creates one automatically the first time it tries to program a
// hostname, so there may not be one yet, and it is matched by suffix: a Domain
// for example.com covers api.example.com. Nothing here creates one — doing so
// would duplicate an object the platform owns. A control plane that does not
// serve Domains at all reads the same as one with no Domain yet: there is
// simply nothing extra to show.
func domainFor(ctx context.Context, c client.Client, hostname string) *networkingv1alpha.Domain {
	var list networkingv1alpha.DomainList
	if err := c.List(ctx, &list, client.InNamespace(util.ResourceNamespace)); err != nil {
		return nil
	}

	var best *networkingv1alpha.Domain
	for i := range list.Items {
		name := normalizeHostname(list.Items[i].Spec.DomainName)
		if name == "" || !covers(name, hostname) {
			continue
		}
		// The most specific Domain wins: a project may hold both example.com
		// and api.example.com, and the latter is the one being verified.
		if best == nil || len(name) > len(normalizeHostname(best.Spec.DomainName)) {
			best = &list.Items[i]
		}
	}
	return best
}

// covers reports whether a domain name covers a hostname, by the platform's own
// rule: an exact match, or a suffix match on a label boundary.
func covers(domainName, hostname string) bool {
	h := normalizeHostname(hostname)
	return h == domainName || strings.HasSuffix(h, "."+domainName)
}
