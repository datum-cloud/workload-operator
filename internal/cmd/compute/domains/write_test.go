// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.datum.net/compute/internal/cmd/compute/util"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// failingClient answers reads normally and refuses one write, which is what a
// conflicting concurrent deploy or a missing update permission looks like.
func failingClient(t *testing.T, boom error, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&networkingv1alpha.HTTPProxy{}, &networkingv1alpha.NetworkService{}, &networkingv1alpha.Domain{}).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return boom
			},
		}).
		Build()
}

// TestAddReportsAWriteFailure: a hostname that could not be attached must fail
// the command and say so. The wait that follows is the part that looks like
// progress, so falling into it after a failed write would show a user a
// verification that is never going to happen.
func TestAddReportsAWriteFailure(t *testing.T) {
	boom := errors.New("Operation cannot be fulfilled on httpproxies.networking.datumapis.com \"api\": the object has been modified")
	c := failingClient(t, boom, programmedProxy(testWorkload, testCanonical),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)))

	var out bytes.Buffer
	err := runAdd(context.Background(), &out, c, testProject, testWorkload, testCustom)
	if err == nil {
		t.Fatal("a hostname that could not be attached must fail the command")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the server's message to reach the user", err)
	}
	for _, want := range []string{testCustom, testWorkload} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	if strings.Contains(out.String(), "Verifying") {
		t.Errorf("nothing was attached, so nothing may be reported as verifying:\n%s", out.String())
	}
	if got := hostnamesOf(t, c); len(got) != 0 {
		t.Errorf("hostnames = %v, want none attached", got)
	}
}

// TestRemoveReportsAWriteFailure: the same on the way out. A detach that did
// not happen must not print the "removed" summary, which is what a user reads
// as confirmation that the hostname is free to attach elsewhere.
func TestRemoveReportsAWriteFailure(t *testing.T) {
	boom := errors.New("forbidden")
	c := failingClient(t, boom, programmedProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)))

	var out bytes.Buffer
	err := runRemove(context.Background(), &out, c, testProject, testWorkload, testCustom, true)
	if err == nil {
		t.Fatal("a hostname that could not be detached must fail the command")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the server's failure", err)
	}
	if strings.Contains(out.String(), "removed from workload") {
		t.Errorf("nothing was detached, so nothing may claim it was:\n%s", out.String())
	}
	if got := hostnamesOf(t, c); len(got) != 1 || got[0] != testCustom {
		t.Errorf("hostnames = %v, want the hostname still attached", got)
	}
}

// TestAddKeepsTheHostnamesAlreadyThere: attaching a second domain must not
// disturb the first. `domains add` writes the whole spec back, so this is the
// same class of mistake a redeploy makes, one command over.
func TestAddKeepsTheHostnamesAlreadyThere(t *testing.T) {
	c := newFakeClient(t,
		programmedProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)),
	)

	// The wait is not what this test is about, and a hostname the platform has
	// not reported on stays pending forever: detach after the first poll.
	var out bytes.Buffer
	if err := runAdd(cancelled(), &out, c, testProject, testWorkload, "www."+testApex); err != nil {
		t.Fatalf("runAdd: %v", err)
	}

	if want := testCustom + ",www." + testApex; strings.Join(hostnamesOf(t, c), ",") != want {
		t.Errorf("hostnames = %v, want %q — declared order, nothing lost", hostnamesOf(t, c), want)
	}
}

// TestRemoveKeepsTheOrderOfWhatIsLeft: removing the middle hostname of three
// must leave the other two in the order they were declared, since that order
// is what every view renders and what `add` appends to.
func TestRemoveKeepsTheOrderOfWhatIsLeft(t *testing.T) {
	c := newFakeClient(t,
		programmedProxy(testWorkload, testCanonical, "one."+testApex, "two."+testApex, "three."+testApex),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)),
	)

	var out bytes.Buffer
	if err := runRemove(context.Background(), &out, c, testProject, testWorkload, "two."+testApex, true); err != nil {
		t.Fatalf("runRemove: %v", err)
	}

	if want := "one." + testApex + ",three." + testApex; strings.Join(hostnamesOf(t, c), ",") != want {
		t.Errorf("hostnames = %v, want %q", hostnamesOf(t, c), want)
	}
}

// TestRemoveTheLastCustomHostname: the workload falls back to serving on its
// Datum-managed domain alone, and is told so — that sentence is the difference
// between "I have taken my site down" and "I have detached my domain".
func TestRemoveTheLastCustomHostname(t *testing.T) {
	c := newFakeClient(t,
		programmedProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)),
	)

	var out bytes.Buffer
	if err := runRemove(context.Background(), &out, c, testProject, testWorkload, testCustom, true); err != nil {
		t.Fatalf("runRemove: %v", err)
	}

	if got := hostnamesOf(t, c); len(got) != 0 {
		t.Errorf("hostnames = %v, want none", got)
	}
	if !strings.Contains(out.String(), "Still serving on https://"+testCanonical) {
		t.Errorf("removing the last custom domain must say what still answers:\n%s", out.String())
	}

	// And the list view still has a row for the workload: the managed domain
	// is permanent.
	var list bytes.Buffer
	if err := runList(context.Background(), &list, c, testProject, util.OutputTable, false); err != nil {
		t.Fatalf("runList: %v", err)
	}
	f := row(t, list.String(), testWorkload, testCanonical)
	if f[len(f)-1] != managedMarker {
		t.Errorf("row = %v, want the managed domain still listed", f)
	}
}

// TestListPropagatesLookupFailures: `domains` exists to answer "is my URL
// working". An unreadable control plane must say so rather than render an
// empty list, which reads as "you have no domains".
func TestListPropagatesLookupFailures(t *testing.T) {
	boom := errors.New("connection refused")
	c := interceptor.NewClient(newFakeClient(t,
		programmedProxy(testWorkload, testCanonical, testCustom),
	), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return boom
		},
	})

	var out bytes.Buffer
	err := runList(context.Background(), &out, c, testProject, util.OutputTable, false)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the lookup failure", err)
	}
	if strings.Contains(out.String(), "No domains in project") {
		t.Errorf("an unreadable project must not be reported as an empty one:\n%s", out.String())
	}
}

// TestDetailPropagatesLookupFailures: the same for the detail view, which is
// the one a user reaches for precisely when something is wrong.
func TestDetailPropagatesLookupFailures(t *testing.T) {
	boom := errors.New("connection refused")
	c := interceptor.NewClient(newFakeClient(t, workload(testWorkload)), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return boom
		},
	})

	var out bytes.Buffer
	err := runDetail(context.Background(), &out, c, testProject, testWorkload, util.OutputTable)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the lookup failure", err)
	}
	if strings.Contains(err.Error(), "--http-port") {
		t.Errorf("a failed lookup must not be reported as a workload that was never published: %v", err)
	}
}
