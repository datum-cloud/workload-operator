// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.datum.net/compute/internal/cmd/compute/url"
	"go.datum.net/compute/internal/cmd/compute/util"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// unverifiedDomain returns a Domain in the shape the platform reports before
// the user has published the TXT record it asked for.
func unverifiedDomain(domainName, recordName, content string) *networkingv1alpha.Domain {
	return &networkingv1alpha.Domain{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.ReplaceAll(domainName, ".", "-"),
			Namespace: util.ResourceNamespace,
		},
		Spec: networkingv1alpha.DomainSpec{DomainName: domainName},
		Status: networkingv1alpha.DomainStatus{
			Verification: &networkingv1alpha.DomainVerificationStatus{
				DNSRecord: networkingv1alpha.DNSVerificationRecord{
					Name:    recordName,
					Type:    "TXT",
					Content: content,
				},
			},
			Conditions: []metav1.Condition{
				cond(networkingv1alpha.DomainConditionVerified, metav1.ConditionFalse, "RecordNotFound", "verification record not found"),
			},
		},
	}
}

// cancelled returns a context that is already done, so a wait runs exactly one
// poll and then takes the detach path.
func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestAddAttachesHostname(t *testing.T) {
	// The platform has already checked this hostname out — the user is
	// re-attaching one it knows — so the wait ends on its first poll and the
	// command reaches its URL. A hostname the platform has said nothing about
	// is pending, and that wait is TestAddPrintsWhatTheServerReports.
	proxy := verifiedProxy(testWorkload, testCanonical, testCustom)
	proxy.Spec.Hostnames = nil

	c := newFakeClient(t,
		proxy,
		serviceWith(testWorkload, 8080, location("DFW", 2, 2, true)),
	)

	var out bytes.Buffer
	if err := runAdd(context.Background(), &out, c, testProject, testWorkload, "HTTPS://API.Example.com./"); err != nil {
		t.Fatalf("runAdd: %v", err)
	}

	// Stored normalized, so the platform sees the name it can validate.
	if got := hostnamesOf(t, c); len(got) != 1 || got[0] != testCustom {
		t.Errorf("hostnames = %v, want [%s]", got, testCustom)
	}

	// The URL is the deliverable, and it is the last thing printed.
	if !strings.Contains(out.String(), "https://"+testCustom) {
		t.Errorf("missing the URL:\n%s", out.String())
	}
}

func TestAddIsIdempotent(t *testing.T) {
	c := newFakeClient(t,
		verifiedProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 2, 2, true)),
	)

	var out bytes.Buffer
	if err := runAdd(context.Background(), &out, c, testProject, testWorkload, testCustom); err != nil {
		t.Fatalf("runAdd: %v", err)
	}

	if got := hostnamesOf(t, c); len(got) != 1 {
		t.Errorf("hostnames = %v, want no duplicate", got)
	}
	if !strings.Contains(out.String(), "already attached") {
		t.Errorf("re-adding should say so plainly:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "https://"+testCustom) {
		t.Errorf("re-adding should still end at the URL:\n%s", out.String())
	}
}

func TestAddErrors(t *testing.T) {
	tests := []struct {
		name     string
		objects  []client.Object
		workload string
		hostname string
		want     string
	}{
		{
			name:     "the Datum-managed hostname is not a custom domain",
			objects:  []client.Object{programmedProxy(testWorkload, testCanonical)},
			workload: testWorkload,
			hostname: "https://" + strings.ToUpper(testCanonical) + "/",
			want:     "already the Datum-managed hostname",
		},
		{
			name:     "wildcard",
			objects:  []client.Object{programmedProxy(testWorkload, testCanonical)},
			workload: testWorkload,
			hostname: "*.example.com",
			want:     "wildcard hostnames are not supported",
		},
		{
			name:     "not a hostname",
			objects:  []client.Object{programmedProxy(testWorkload, testCanonical)},
			workload: testWorkload,
			hostname: "localhost",
			want:     "not a fully qualified hostname",
		},
		{
			name:     "workload has no URL to attach to",
			objects:  []client.Object{workload(testWorkload)},
			workload: testWorkload,
			hostname: testCustom,
			want:     wantPublishHint,
		},
		{
			name:     "workload does not exist",
			objects:  nil,
			workload: "ghost",
			hostname: testCustom,
			want:     "not found in project " + testProject,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClient(t, tc.objects...)
			var out bytes.Buffer
			err := runAdd(context.Background(), &out, c, testProject, tc.workload, tc.hostname)
			if err == nil {
				t.Fatalf("expected an error, got output:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A hostname that is not live yet prints the records the server asked for, the
// checkmarks for the checks that have passed, and nothing it made up.
func TestAddPrintsWhatTheServerReports(t *testing.T) {
	c := newFakeClient(t,
		pendingProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 2, 2, true)),
		unverifiedDomain(testApex, "_datum."+testApex, "datum-verify=8f3a91c2b7"),
	)

	var out bytes.Buffer
	if err := waitForHostname(cancelled(), &out, c, testWorkload, testCustom); err != nil {
		t.Fatalf("waitForHostname: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "Verifying "+testApex+"...") {
		t.Errorf("should name the domain being verified:\n%s", got)
	}
	if !strings.Contains(got, "Add these DNS records:") {
		t.Errorf("missing the records block:\n%s", got)
	}
	if f := row(t, got, "TYPE"); strings.Join(f, " ") != "TYPE NAME VALUE" {
		t.Errorf("records header = %v", f)
	}
	if f := row(t, got, "TXT"); strings.Join(f[1:], " ") != "_datum."+testApex+" datum-verify=8f3a91c2b7" {
		t.Errorf("TXT row = %v, want the server's own record", f)
	}
	if f := row(t, got, "CNAME"); strings.Join(f[1:], " ") != testCustom+" "+testCanonical {
		t.Errorf("CNAME row = %v, want it to point at the platform hostname", f)
	}

	if !strings.Contains(got, "Ctrl-C to detach") {
		t.Errorf("should say Ctrl-C is safe:\n%s", got)
	}

	// Only the check that passed gets a checkmark.
	if !strings.Contains(got, "Domain verified") {
		t.Errorf("missing the verified checkmark:\n%s", got)
	}
	if strings.Contains(got, "Certificate issued") {
		t.Errorf("a pending certificate must not be reported as issued:\n%s", got)
	}
	// And the URL is not offered before it works.
	if strings.Contains(got, "\n  https://"+testCustom) {
		t.Errorf("a hostname that is not live should not be printed as a URL:\n%s", got)
	}
}

func TestAddDetachOnInterruptIsNotAnError(t *testing.T) {
	c := newFakeClient(t,
		pendingProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 2, 2, true)),
	)

	var out bytes.Buffer
	if err := waitForHostname(cancelled(), &out, c, testWorkload, testCustom); err != nil {
		t.Fatalf("detaching returned an error: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "Detached.") {
		t.Errorf("missing the detach note:\n%s", got)
	}
	if !strings.Contains(got, "datumctl compute domains") {
		t.Errorf("detaching should point at the command that shows the state:\n%s", got)
	}
}

func TestRecordsFor(t *testing.T) {
	verified := unverifiedDomain(testApex, "_datum."+testApex, "token")
	verified.Status.Conditions = []metav1.Condition{
		cond(networkingv1alpha.DomainConditionVerified, metav1.ConditionTrue, "Verified", ""),
	}

	programmedHostname := &url.Hostname{
		Hostname: testCustom,
		Conditions: []metav1.Condition{
			cond(networkingv1alpha.HostnameConditionDNSRecordProgrammed, metav1.ConditionTrue, "RecordCreated", ""),
		},
	}

	tests := []struct {
		name     string
		info     *url.Info
		hostname *url.Hostname
		domain   *networkingv1alpha.Domain
		want     []string
	}{
		{
			name:     "both records outstanding",
			info:     &url.Info{CanonicalHostname: testCanonical},
			hostname: &url.Hostname{Hostname: testCustom},
			domain:   unverifiedDomain(testApex, "_datum."+testApex, "token"),
			want:     []string{"TXT _datum." + testApex + " token", "CNAME " + testCustom + " " + testCanonical},
		},
		{
			name:     "an already verified domain needs no TXT",
			info:     &url.Info{CanonicalHostname: testCanonical},
			hostname: &url.Hostname{Hostname: testCustom},
			domain:   verified,
			want:     []string{"CNAME " + testCustom + " " + testCanonical},
		},
		{
			name:     "a record the platform programmed itself needs no CNAME",
			info:     &url.Info{CanonicalHostname: testCanonical},
			hostname: programmedHostname,
			domain:   verified,
			want:     nil,
		},
		{
			name:     "no Domain resource yet",
			info:     &url.Info{CanonicalHostname: testCanonical},
			hostname: &url.Hostname{Hostname: testCustom},
			domain:   nil,
			want:     []string{"CNAME " + testCustom + " " + testCanonical},
		},
		{
			name:     "no platform hostname assigned yet, so nothing to point at",
			info:     &url.Info{},
			hostname: &url.Hostname{Hostname: testCustom},
			domain:   nil,
			want:     nil,
		},
		{
			name:     "a Domain with no verification record reported",
			info:     &url.Info{},
			hostname: &url.Hostname{Hostname: testCustom},
			domain: &networkingv1alpha.Domain{
				Spec: networkingv1alpha.DomainSpec{DomainName: testApex},
			},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range recordsFor(tc.info, tc.hostname, tc.domain) {
				got = append(got, strings.Join([]string{r.Type, r.Name, r.Value}, " "))
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("records = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDomainForPicksTheMostSpecific(t *testing.T) {
	c := newFakeClient(t,
		unverifiedDomain(testApex, "_datum."+testApex, "apex"),
		unverifiedDomain(testCustom, "_datum."+testCustom, "sub"),
		unverifiedDomain("other.com", "_datum.other.com", "other"),
	)

	got := domainFor(context.Background(), c, "https://API.Example.com./")
	if got == nil || got.Spec.DomainName != testCustom {
		t.Fatalf("domainFor = %v, want %s", got, testCustom)
	}

	if got := domainFor(context.Background(), c, "www."+testApex); got == nil || got.Spec.DomainName != testApex {
		t.Errorf("domainFor(www.%s) = %v, want the apex Domain", testApex, got)
	}
	if got := domainFor(context.Background(), c, "nothing.test"); got != nil {
		t.Errorf("domainFor(nothing.test) = %v, want nil", got)
	}
}

func TestCovers(t *testing.T) {
	tests := []struct {
		domain, hostname string
		want             bool
	}{
		{testApex, testApex, true},
		{testApex, testCustom, true},
		{testApex, "foo." + testCustom, true},
		{testApex, "test-example.com", false},
		{testApex, "notexample.com", false},
		{testCustom, testApex, false},
	}

	for _, tc := range tests {
		t.Run(tc.domain+"/"+tc.hostname, func(t *testing.T) {
			if got := covers(tc.domain, tc.hostname); got != tc.want {
				t.Errorf("covers(%q, %q) = %v, want %v", tc.domain, tc.hostname, got, tc.want)
			}
		})
	}
}

func TestHostnameLive(t *testing.T) {
	tests := []struct {
		name string
		h    *url.Hostname
		want bool
	}{
		{"nil", nil, false},
		{"not serving", &url.Hostname{Active: false}, false},
		{
			name: "serving, certificate issued",
			h: &url.Hostname{Active: true, Conditions: []metav1.Condition{
				cond(networkingv1alpha.HostnameConditionCertificateReady, metav1.ConditionTrue, "CertificateIssued", ""),
			}},
			want: true,
		},
		{
			name: "serving, certificate still coming",
			h: &url.Hostname{Active: true, Conditions: []metav1.Condition{
				cond(networkingv1alpha.HostnameConditionCertificateReady, metav1.ConditionFalse, "Pending", ""),
			}},
			want: false,
		},
		{
			name: "serving, and the platform said nothing about a certificate",
			h:    &url.Hostname{Active: true},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostnameLive(tc.h); got != tc.want {
				t.Errorf("hostnameLive = %v, want %v", got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------
// what gets announced, and when
// -----------------------------------------------------------------------

// TestAddPrintsNothingBeforeThePlatformHasAnythingToSay: the first poll runs
// moments after the attach, before the platform has created the domain behind
// the hostname or computed the record that proves the user owns it. Announcing
// then prints a records block with the verification record missing from it —
// and the block is printed once, so that record would never reach the user.
func TestAddPrintsNothingBeforeThePlatformHasAnythingToSay(t *testing.T) {
	// No Domain object: the platform has not got to this hostname yet.
	c := newFakeClient(t,
		pendingProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 2, 2, true)),
	)

	var out bytes.Buffer
	if err := waitForHostname(cancelled(), &out, c, testWorkload, testCustom); err != nil {
		t.Fatalf("waitForHostname: %v", err)
	}
	got := out.String()

	for _, unwanted := range []string{"Verifying", "Add these DNS records", "Waiting for DNS", "Domain verified"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("nothing has been published yet, so nothing should be announced — found %q in:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "Detached.") {
		t.Errorf("missing the detach note:\n%s", got)
	}
}

// TestAddPrintsTheVerificationRecordWhenItAppears: the TXT record is the whole
// custom-domain flow. It shows up a poll or two after the attach, and it has to
// be printed when it does — being told to wait for DNS records that were never
// shown is the failure this guards.
func TestAddPrintsTheVerificationRecordWhenItAppears(t *testing.T) {
	info := &url.Info{WorkloadName: testWorkload, CanonicalHostname: testCanonical}
	h := &url.Hostname{Hostname: testCustom, URL: "https://" + testCustom}

	var out bytes.Buffer
	w := &hostnameWatch{out: &out, hostname: testCustom, started: time.Now(), seen: map[string]bool{}}

	// First poll: the platform has not created the Domain yet.
	announced, err := w.announce(info, h, nil)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if announced {
		t.Errorf("announced before the platform had published anything:\n%s", out.String())
	}
	if out.Len() != 0 {
		t.Errorf("nothing to announce yet, but printed:\n%s", out.String())
	}

	// The platform catches up, and asks for the record it needs.
	domain := unverifiedDomain(testApex, "_datum."+testApex, "datum-verify=8f3a91c2b7")
	announced, err = w.announce(info, h, domain)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if !announced {
		t.Fatalf("the records are published — they must be announced:\n%s", out.String())
	}
	got := out.String()

	if !strings.Contains(got, "Verifying "+testApex+"...") {
		t.Errorf("should name the domain being verified:\n%s", got)
	}
	if f := row(t, got, "TXT"); strings.Join(f[1:], " ") != "_datum."+testApex+" datum-verify=8f3a91c2b7" {
		t.Errorf("TXT row = %v, want the record the platform asked for", f)
	}
	if f := row(t, got, "CNAME"); strings.Join(f[1:], " ") != testCustom+" "+testCanonical {
		t.Errorf("CNAME row = %v", f)
	}
	if !strings.Contains(got, "Ctrl-C to detach") {
		t.Errorf("should say Ctrl-C is safe:\n%s", got)
	}

	// And nothing is said twice on the polls that follow.
	before := out.Len()
	if _, err := w.announce(info, h, domain); err != nil {
		t.Fatalf("announce: %v", err)
	}
	if out.Len() != before {
		t.Errorf("announced twice:\n%s", out.String()[before:])
	}
}

// TestAddSaysWhenNoVerificationRecordArrives: waiting quietly is only defensible
// while the platform is still working. Past that, whatever it did publish is
// printed along with the fact that the rest has not been.
func TestAddSaysWhenNoVerificationRecordArrives(t *testing.T) {
	info := &url.Info{WorkloadName: testWorkload, CanonicalHostname: testCanonical}
	h := &url.Hostname{Hostname: testCustom, URL: "https://" + testCustom}

	var out bytes.Buffer
	w := &hostnameWatch{
		out:      &out,
		hostname: testCustom,
		started:  time.Now().Add(-2 * recordWindow),
		seen:     map[string]bool{},
	}

	announced, err := w.announce(info, h, nil)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if !announced {
		t.Fatalf("there is a record to print, so it must be printed:\n%s", out.String())
	}
	got := out.String()

	if f := row(t, got, "CNAME"); strings.Join(f[1:], " ") != testCustom+" "+testCanonical {
		t.Errorf("CNAME row = %v, want what the platform did publish", f)
	}
	if !strings.Contains(got, "has not published a verification record") {
		t.Errorf("a missing verification record must be said out loud:\n%s", got)
	}

	// And when it finally turns up, it is still printed.
	domain := unverifiedDomain(testApex, "_datum."+testApex, "late")
	if _, err := w.announce(info, h, domain); err != nil {
		t.Fatalf("announce: %v", err)
	}
	if f := row(t, out.String(), "TXT"); strings.Join(f[1:], " ") != "_datum."+testApex+" late" {
		t.Errorf("TXT row = %v, want the record printed once it exists", f)
	}
}

// TestAddStopsWhenThePlatformPublishesNothing: a wait with nothing to wait for
// and nothing to show is a hang. It ends, and says why.
func TestAddStopsWhenThePlatformPublishesNothing(t *testing.T) {
	// No managed hostname to point at, and no Domain: there is no record this
	// command could print, so there is nothing for the user to do.
	info := &url.Info{WorkloadName: testWorkload}
	h := &url.Hostname{Hostname: testCustom, URL: "https://" + testCustom}

	var out bytes.Buffer
	w := &hostnameWatch{
		out:      &out,
		hostname: testCustom,
		started:  time.Now().Add(-2 * recordWindow),
		seen:     map[string]bool{},
	}

	announced, err := w.announce(info, h, nil)
	if err == nil {
		t.Fatalf("a wait with nothing to wait for must end, got output:\n%s", out.String())
	}
	if announced {
		t.Errorf("nothing was published, so nothing was announced")
	}
	for _, want := range []string{testCustom, testWorkload, "datumctl compute domains"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestAddWaitFailsWhenNothingIsEverPublished: and the failure reaches the
// command, rather than being swallowed by the poll loop.
func TestAddWaitFailsWhenNothingIsEverPublished(t *testing.T) {
	proxy := programmedProxy(testWorkload, "", testCustom)
	c := newFakeClient(t, proxy, serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)))

	w := &hostnameWatch{
		out:      io.Discard,
		hostname: testCustom,
		started:  time.Now().Add(-2 * recordWindow),
		seen:     map[string]bool{},
	}
	done, err := w.check(context.Background(), c, testWorkload)
	if err == nil {
		t.Fatal("the poll loop must surface a wait that can never finish")
	}
	if done {
		t.Error("the hostname is not live")
	}
}

// -----------------------------------------------------------------------
// how many custom hostnames a workload may have
// -----------------------------------------------------------------------

// TestAddAllowsSeveralCustomHostnames: principle 6 is one workload, one URL —
// one routing target, not a cap on names. Every comparable CLI appends, the
// Datum-managed hostname already sits alongside whatever the user attached, and
// replacing a production hostname because someone added a staging alias would
// be a surprise no platform in the table commits. So `add` appends, and says
// what the workload serves now.
func TestAddAllowsSeveralCustomHostnames(t *testing.T) {
	c := newFakeClient(t,
		programmedProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)),
	)

	var out bytes.Buffer
	if err := runAdd(cancelled(), &out, c, testProject, testWorkload, "www."+testApex); err != nil {
		t.Fatalf("runAdd: %v", err)
	}
	got := out.String()

	if want := testCustom + ",www." + testApex; strings.Join(hostnamesOf(t, c), ",") != want {
		t.Fatalf("hostnames = %v, want %q — the first one is not replaced", hostnamesOf(t, c), want)
	}
	if !strings.Contains(got, "www."+testApex+" attached to workload") {
		t.Errorf("attaching must say it attached:\n%s", got)
	}
	if !strings.Contains(got, "2 custom hostnames") {
		t.Errorf("a workload serving more than one hostname must say so:\n%s", got)
	}
}

// TestAddHelpAgreesThatSeveralHostnamesAreAllowed: the behavior above is only
// coherent if the help says it. A user who expects `add` to replace has to be
// told otherwise before they run it, not after.
func TestAddHelpAgreesThatSeveralHostnamesAreAllowed(t *testing.T) {
	long := addCommand().Long

	for _, want := range []string{"several custom hostnames", "never replaces", "domains remove"} {
		if !strings.Contains(long, want) {
			t.Errorf("help must mention %q:\n%s", want, long)
		}
	}
}
