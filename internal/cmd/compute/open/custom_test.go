// SPDX-License-Identifier: AGPL-3.0-only

package open

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.datum.net/compute/internal/cmd/compute/url"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const testCustom = "api.example.com"

// withCustomHostname returns the objects for a workload serving on a custom
// hostname as well as its managed one. active decides whether the custom
// hostname is the one answering yet.
func withCustomHostname(t *testing.T, active bool) []client.Object {
	t.Helper()
	objs := publishedObjects(t, testCanonical, true)

	proxy := objs[1].(*networkingv1alpha.HTTPProxy)
	proxy.Spec.Hostnames = []gatewayv1.Hostname{testCustom}

	status := metav1.ConditionFalse
	reason := "DomainNotVerified"
	if active {
		status = metav1.ConditionTrue
		reason = "Verified"
	}
	proxy.Status.HostnameStatuses = []networkingv1alpha.HostnameStatus{{
		Hostname: testCustom,
		Conditions: []metav1.Condition{
			{Type: networkingv1alpha.HostnameConditionVerified, Status: status, Reason: reason,
				Message: "waiting for the TXT record"},
			{Type: networkingv1alpha.HostnameConditionCertificateReady, Status: status, Reason: "CertificateIssued"},
		},
	}}
	return objs
}

// TestOpenPrefersTheCustomHostname: once a custom domain is serving, it is the
// address the user thinks of as their site, and it is the one a browser gets.
// The managed hostname still works and is still listed by `domains`; it is
// just not what `open` is for.
func TestOpenPrefersTheCustomHostname(t *testing.T) {
	c := newFakeClient(t, withCustomHostname(t, true)...)
	opener := &recordingOpener{}

	var out strings.Builder
	if err := run(context.Background(), &out, c, testWorkload, false, opener.open); err != nil {
		t.Fatalf("run: %v", err)
	}

	want := "https://" + testCustom
	if len(opener.calls) != 1 || opener.calls[0] != want {
		t.Errorf("opened %v, want %q", opener.calls, want)
	}
	if !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want it to name the custom hostname", out.String())
	}
	if strings.Contains(out.String(), testCanonical) {
		t.Errorf("output = %q, should not offer the managed hostname once a custom one serves", out.String())
	}
}

// TestOpenFallsBackWhileACustomHostnameIsPending: a hostname that has not
// verified does not answer, so opening it would show the user a browser error
// for a workload that is running perfectly. The managed hostname is what works.
func TestOpenFallsBackWhileACustomHostnameIsPending(t *testing.T) {
	c := newFakeClient(t, withCustomHostname(t, false)...)
	opener := &recordingOpener{}

	var out strings.Builder
	if err := run(context.Background(), &out, c, testWorkload, false, opener.open); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(opener.calls) != 1 || opener.calls[0] != testURL {
		t.Errorf("opened %v, want the managed URL %q while the custom hostname is pending", opener.calls, testURL)
	}
}

// TestOpenURLMatchesWhatDomainsShows is the cross-command contract: the URL
// `open --url` prints is exactly the one the domains detail view reports, so a
// user never has two answers to "what is my URL".
func TestOpenURLMatchesWhatDomainsShows(t *testing.T) {
	c := newFakeClient(t, withCustomHostname(t, true)...)

	info, err := url.ForWorkload(context.Background(), c, testWorkload)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	var out strings.Builder
	if err := run(context.Background(), &out, c, testWorkload, true, (&recordingOpener{}).open); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != info.URL {
		t.Errorf("open --url printed %q, but the URL info says %q", got, info.URL)
	}
}

// A workload the CLI cannot read at all is a different problem from one that
// does not exist, and must not be reported as either "not found" or "no URL".
func TestOpenReportsAnUnreadableWorkload(t *testing.T) {
	boom := errors.New("workloads.compute.datumapis.com is forbidden")
	c := interceptor.NewClient(newFakeClient(t), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	})

	opener := &recordingOpener{}
	var out strings.Builder
	err := run(context.Background(), &out, c, testWorkload, false, opener.open)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the server's failure", err)
	}
	if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "--http-port") {
		t.Errorf("a permission failure must not read as a missing or unpublished workload: %v", err)
	}
	if len(opener.calls) != 0 {
		t.Errorf("opened %v, want nothing", opener.calls)
	}
}
