// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"bytes"
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/internal/cmd/compute/url"
	"go.datum.net/compute/internal/cmd/compute/util"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// Fixtures shared by the tests in this package.
const (
	testProject  = "acme-prod"
	testWorkload = "api"
	testPortName = "http"

	testCanonical = "a1b2c3d4.datumproxy.net"
	testApex      = "example.com"
	testCustom    = "api." + testApex

	// wantPublishHint is what an unpublished workload's error has to point at:
	// the flag that gives it a URL in the first place.
	wantPublishHint = "--http-port"

	// wantNoHostname is the error for a command called without a hostname.
	wantNoHostname = "no hostname given"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := computev1alpha.AddToScheme(s); err != nil {
		t.Fatalf("registering compute scheme: %v", err)
	}
	if err := networkingv1alpha.AddToScheme(s); err != nil {
		t.Fatalf("registering networking scheme: %v", err)
	}
	return s
}

func newFakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&networkingv1alpha.HTTPProxy{}, &networkingv1alpha.NetworkService{}, &networkingv1alpha.Domain{}).
		WithObjects(objs...).
		Build()
}

func workload(name string) *computev1alpha.Workload {
	return &computev1alpha.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: util.ResourceNamespace,
			UID:       types.UID("uid-" + name),
		},
	}
}

func cond(condType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: condType, Status: status, Reason: reason, Message: message}
}

// programmedProxy returns a proxy in the shape the platform reports once it is
// serving: accepted, programmed, and holding a certificate.
func programmedProxy(name, canonical string, hostnames ...string) *networkingv1alpha.HTTPProxy {
	p := url.BuildHTTPProxy(workload(name), testPortName, hostnames)
	p.Status.CanonicalHostname = canonical
	p.Status.Conditions = []metav1.Condition{
		cond(networkingv1alpha.HTTPProxyConditionAccepted, metav1.ConditionTrue, "Accepted", ""),
		cond(networkingv1alpha.HTTPProxyConditionProgrammed, metav1.ConditionTrue, "Programmed", ""),
		cond(networkingv1alpha.HTTPProxyConditionCertificatesReady, metav1.ConditionTrue, "AllCertificatesReady", ""),
	}
	return p
}

// verifiedProxy returns a proxy whose custom hostname the platform has already
// checked and is serving: verified, its DNS record programmed, its certificate
// issued. A hostname is only ever active on the strength of its own entry
// here — the proxy-level conditions are a roll-up over every hostname and
// cannot vouch for any one of them.
func verifiedProxy(name, canonical, hostname string) *networkingv1alpha.HTTPProxy {
	p := programmedProxy(name, canonical, hostname)
	p.Status.HostnameStatuses = []networkingv1alpha.HostnameStatus{{
		Hostname: hostname,
		Conditions: []metav1.Condition{
			cond(networkingv1alpha.HostnameConditionVerified, metav1.ConditionTrue, "Verified", ""),
			cond(networkingv1alpha.HostnameConditionAvailable, metav1.ConditionTrue, "Claimed", ""),
			cond(networkingv1alpha.HostnameConditionDNSRecordProgrammed, metav1.ConditionTrue, "RecordCreated", ""),
			cond(networkingv1alpha.HostnameConditionCertificateReady, metav1.ConditionTrue, "CertificateIssued", ""),
		},
	}}
	return p
}

// pendingProxy returns a proxy whose custom hostname is still waiting on DNS.
func pendingProxy(name, canonical, hostname string) *networkingv1alpha.HTTPProxy {
	p := programmedProxy(name, canonical, hostname)
	p.Status.HostnameStatuses = []networkingv1alpha.HostnameStatus{{
		Hostname: hostname,
		Conditions: []metav1.Condition{
			cond(networkingv1alpha.HostnameConditionVerified, metav1.ConditionTrue, "Verified", ""),
			cond(networkingv1alpha.HostnameConditionDNSRecordProgrammed, metav1.ConditionFalse, "Pending", "waiting for DNS"),
			cond(networkingv1alpha.HostnameConditionCertificateReady, metav1.ConditionFalse, "ChallengeInProgress", "ACME challenge in progress"),
		},
	}}
	return p
}

func serviceWith(name string, port int32, locations ...networkingv1alpha.NetworkServiceLocationStatus) *networkingv1alpha.NetworkService {
	s := url.BuildNetworkService(workload(name), testPortName, port)
	var members, healthy int32
	for _, l := range locations {
		members += l.Members
		healthy += l.Healthy
	}
	s.Status.Summary = networkingv1alpha.NetworkServiceSummary{
		Locations: int32(len(locations)),
		Members:   members,
		Healthy:   healthy,
	}
	s.Status.Locations = locations
	s.Status.Conditions = []metav1.Condition{
		cond(networkingv1alpha.NetworkServiceMembersResolved, metav1.ConditionTrue, "MembersResolved", ""),
		cond(networkingv1alpha.NetworkServiceReady, metav1.ConditionTrue, "Ready", ""),
	}
	return s
}

func location(city string, members, healthy int32, serving bool) networkingv1alpha.NetworkServiceLocationStatus {
	return networkingv1alpha.NetworkServiceLocationStatus{Name: city, Members: members, Healthy: healthy, Serving: serving}
}

// row returns the whitespace-separated fields of the first line whose fields
// begin with the given ones, so assertions never depend on column widths.
func row(t *testing.T, output string, first ...string) []string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < len(first) {
			continue
		}
		match := true
		for i, want := range first {
			if fields[i] != want {
				match = false
				break
			}
		}
		if match {
			return fields
		}
	}
	t.Fatalf("no line starting with %v in:\n%s", first, output)
	return nil
}

// -----------------------------------------------------------------------
// the list view
// -----------------------------------------------------------------------

func TestListRendersEveryHostname(t *testing.T) {
	c := newFakeClient(t,
		verifiedProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 2, 2, true)),
		pendingProxy("web", "e5f6a7b8.datumproxy.net", "www.acme.io"),
		serviceWith("web", 3000, location("DFW", 1, 1, true)),
	)

	var out bytes.Buffer
	if err := runList(context.Background(), &out, c, testProject, util.OutputTable, false); err != nil {
		t.Fatalf("runList: %v", err)
	}
	got := out.String()

	if f := row(t, got, "WORKLOAD"); strings.Join(f, " ") != "WORKLOAD DOMAIN STATUS CERTIFICATE" {
		t.Errorf("header = %v", f)
	}

	// The custom hostname comes before the managed one for the same workload:
	// the name the user chose is the one they are looking for.
	custom := row(t, got, testWorkload, testCustom)
	if strings.Join(custom[2:], " ") != "active valid" {
		t.Errorf("custom row = %v, want active valid", custom)
	}
	if strings.Contains(strings.Join(custom, " "), managedMarker) {
		t.Errorf("custom hostname marked as Datum-managed: %v", custom)
	}

	managedRow := row(t, got, testWorkload, testCanonical)
	if strings.Join(managedRow[2:], " ") != "active valid "+managedMarker {
		t.Errorf("managed row = %v, want the %s marker", managedRow, managedMarker)
	}

	// The server's own words for a hostname that is not serving yet: its
	// messages, never the camelCase reasons behind them.
	pending := strings.Join(row(t, got, "web", "www.acme.io")[2:], " ")
	if pending != "waiting for DNS ACME challenge in progress" {
		t.Errorf("pending row = %q, want the server's messages verbatim", pending)
	}
	if strings.Contains(got, "ChallengeInProgress") {
		t.Errorf("a raw condition reason reached the table:\n%s", got)
	}

	// Workloads are ordered, so two runs read the same.
	if strings.Index(got, "\n  api ") > strings.Index(got, "\n  web ") {
		t.Errorf("workloads out of order:\n%s", got)
	}
}

func TestListManagedHostnameAlwaysAppears(t *testing.T) {
	// A workload with no custom hostname still has a domain: the managed one.
	c := newFakeClient(t,
		programmedProxy(testWorkload, testCanonical),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)),
	)

	var out bytes.Buffer
	if err := runList(context.Background(), &out, c, testProject, util.OutputTable, false); err != nil {
		t.Fatalf("runList: %v", err)
	}

	f := row(t, out.String(), testWorkload, testCanonical)
	if f[len(f)-1] != managedMarker {
		t.Errorf("row = %v, want it marked %s", f, managedMarker)
	}
}

func TestListEmpty(t *testing.T) {
	c := newFakeClient(t)

	var out bytes.Buffer
	if err := runList(context.Background(), &out, c, testProject, util.OutputTable, false); err != nil {
		t.Fatalf("runList: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "No domains in project "+testProject) {
		t.Errorf("missing the empty-state sentence:\n%s", got)
	}
	if !strings.Contains(got, "--http-port") {
		t.Errorf("empty state should say how to get a domain:\n%s", got)
	}
	if strings.Contains(got, "WORKLOAD") {
		t.Errorf("empty state should not print a header row:\n%s", got)
	}
}

func TestListNoHeaders(t *testing.T) {
	c := newFakeClient(t,
		programmedProxy(testWorkload, testCanonical),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)),
	)

	var out bytes.Buffer
	if err := runList(context.Background(), &out, c, testProject, util.OutputTable, true); err != nil {
		t.Fatalf("runList: %v", err)
	}
	if strings.Contains(out.String(), "WORKLOAD") {
		t.Errorf("--no-headers still printed a header:\n%s", out.String())
	}
}

func TestListJSONIsScriptable(t *testing.T) {
	c := newFakeClient(t,
		programmedProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 2, 2, true)),
	)

	var out bytes.Buffer
	if err := runList(context.Background(), &out, c, testProject, util.OutputJSON, false); err != nil {
		t.Fatalf("runList: %v", err)
	}
	got := out.String()

	for _, want := range []string{`"domain": "` + testCustom + `"`, `"url": "https://` + testCustom + `"`, `"managed": true`} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON missing %s:\n%s", want, got)
		}
	}
}

func TestListRowsOrderCustomBeforeManaged(t *testing.T) {
	rows := listRows(map[string]*url.Info{
		"web": {Hostnames: []url.Hostname{{Hostname: "www.acme.io"}}},
		"api": {Hostnames: []url.Hostname{{Hostname: testCustom}, {Hostname: testCanonical, Managed: true}}},
	})

	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.Workload+"/"+r.Domain)
	}
	want := []string{"api/" + testCustom, "api/" + testCanonical, "web/www.acme.io"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// -----------------------------------------------------------------------
// the detail view
// -----------------------------------------------------------------------

func TestDetailDegradedAcrossCities(t *testing.T) {
	c := newFakeClient(t,
		programmedProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 2, 2, true), location("IAD", 2, 0, false)),
	)

	var out bytes.Buffer
	if err := runDetail(context.Background(), &out, c, testProject, testWorkload, util.OutputTable); err != nil {
		t.Fatalf("runDetail: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "https://"+testCustom) || !strings.Contains(got, "https://"+testCanonical) {
		t.Errorf("detail view should list both hostnames:\n%s", got)
	}
	if f := row(t, got, "Backend"); strings.Join(f[1:], " ") != "port 8080/tcp" {
		t.Errorf("backend row = %v", f)
	}
	if !strings.Contains(got, "Degraded — 2 of 4 backends healthy") {
		t.Errorf("missing the health summary:\n%s", got)
	}

	if f := row(t, got, "CITY"); strings.Join(f, " ") != "CITY BACKENDS HEALTHY SERVING" {
		t.Errorf("per-city header = %v", f)
	}
	if f := row(t, got, "DFW"); strings.Join(f[1:], " ") != "2 2 yes" {
		t.Errorf("DFW row = %v", f)
	}
	if f := row(t, got, "IAD"); strings.Join(f[1:], " ") != "2 0 no" {
		t.Errorf("IAD row = %v", f)
	}

	if !strings.Contains(got, "IAD: no healthy backends") {
		t.Errorf("missing the degraded narrative:\n%s", got)
	}
	if !strings.Contains(got, "Next steps:") ||
		!strings.Contains(got, "datumctl compute instances --workload=api --city=IAD") {
		t.Errorf("missing the next-steps block:\n%s", got)
	}
}

func TestDetailHealthyHasNoDiagnosis(t *testing.T) {
	c := newFakeClient(t,
		programmedProxy(testWorkload, testCanonical),
		serviceWith(testWorkload, 8080, location("DFW", 2, 2, true), location("IAD", 2, 2, true)),
	)

	var out bytes.Buffer
	if err := runDetail(context.Background(), &out, c, testProject, testWorkload, util.OutputTable); err != nil {
		t.Fatalf("runDetail: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "Healthy — 4 of 4 backends healthy") {
		t.Errorf("missing the health summary:\n%s", got)
	}
	if strings.Contains(got, "Next steps:") {
		t.Errorf("a healthy URL should not suggest a fix:\n%s", got)
	}
}

func TestDetailJSON(t *testing.T) {
	c := newFakeClient(t,
		programmedProxy(testWorkload, testCanonical),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)),
	)

	var out bytes.Buffer
	if err := runDetail(context.Background(), &out, c, testProject, testWorkload, util.OutputJSON); err != nil {
		t.Fatalf("runDetail: %v", err)
	}
	if !strings.Contains(out.String(), `"url": "https://`+testCanonical+`"`) {
		t.Errorf("JSON detail missing the URL:\n%s", out.String())
	}
}

func TestDetailErrors(t *testing.T) {
	tests := []struct {
		name     string
		objects  []client.Object
		workload string
		want     string
	}{
		{
			name:     "workload exists but was never published",
			objects:  []client.Object{workload(testWorkload)},
			workload: testWorkload,
			want:     wantPublishHint,
		},
		{
			name:     "workload does not exist",
			objects:  nil,
			workload: "ghost",
			want:     "not found in project " + testProject,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClient(t, tc.objects...)
			var out bytes.Buffer
			err := runDetail(context.Background(), &out, c, testProject, tc.workload, util.OutputTable)
			if err == nil {
				t.Fatalf("expected an error, got output:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// hostnamesOf reads back what a proxy declares, for mutation assertions.
func hostnamesOf(t *testing.T, c client.Client) []string {
	t.Helper()
	var proxy networkingv1alpha.HTTPProxy
	key := types.NamespacedName{Namespace: util.ResourceNamespace, Name: testWorkload}
	if err := c.Get(context.Background(), key, &proxy); err != nil {
		t.Fatalf("reading the URL for workload %q: %v", testWorkload, err)
	}
	out := make([]string, 0, len(proxy.Spec.Hostnames))
	for _, h := range proxy.Spec.Hostnames {
		out = append(out, string(h))
	}
	return out
}

func hostnameList(hostnames ...string) []gatewayv1.Hostname {
	out := make([]gatewayv1.Hostname, 0, len(hostnames))
	for _, h := range hostnames {
		out = append(out, gatewayv1.Hostname(h))
	}
	return out
}
