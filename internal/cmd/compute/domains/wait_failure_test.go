// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// TestAddStopsWhenTheHostnameCanNeverBeRead: `domains add` attaches the
// hostname and then waits for the platform to verify it. A control plane the
// wait cannot read is not going to start answering, so swallowing the read
// error leaves the command polling in silence forever — the user is told to
// wait for DNS and never told anything again. It has to give up and say why.
//
// Regression test: before the failure budget, this hung past the guard below.
func TestAddStopsWhenTheHostnameCanNeverBeRead(t *testing.T) {
	boom := errors.New("httpproxies.networking.datumapis.com is forbidden")

	// Reads fail only after the hostname is attached, so the command gets far
	// enough to start waiting — which is the state that used to hang.
	attached := false
	c := interceptor.NewClient(newFakeClient(t,
		programmedProxy(testWorkload, testCanonical),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)),
	), interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			attached = true
			return cl.Update(ctx, obj, opts...)
		},
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if attached {
				return boom
			}
			return cl.List(ctx, list, opts...)
		},
	})

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runAdd(context.Background(), &out, c, testProject, testWorkload, testCustom)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hostname whose state cannot be read must fail the command, not wait forever")
		}
		if !errors.Is(err, boom) {
			t.Errorf("error = %v, want the server's failure to survive wrapping", err)
		}
		if !strings.Contains(err.Error(), testCustom) {
			t.Errorf("error = %q, want it to name the hostname", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("runAdd never returned — the wait is hanging on an unreadable control plane:\n%s", out.String())
	}
}

// TestAddRidesOutATransientReadFailure: the counterpart. One failed read inside
// the wait is nothing — the platform was just asked to check the hostname — and
// giving up there would break a command that is going fine.
//
// The hostname is already attached and verified, so runAdd reports that and goes
// straight to the wait. The first read is the one runAdd itself makes; the
// second is the wait's, and that is the one that fails.
func TestAddRidesOutATransientReadFailure(t *testing.T) {
	reads := 0
	failed := false
	c := interceptor.NewClient(newFakeClient(t,
		verifiedProxy(testWorkload, testCanonical, testCustom),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)),
	), interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*networkingv1alpha.HTTPProxyList); ok {
				reads++
				if reads == 2 {
					failed = true
					return errors.New("etcdserver: request timed out")
				}
			}
			return cl.List(ctx, list, opts...)
		},
	})

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runAdd(context.Background(), &out, c, testProject, testWorkload, testCustom)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("one transient read failure must not fail the command: %v\n%s", err, out.String())
		}
		if !failed {
			t.Error("the interceptor never injected its failure, so nothing was ridden out")
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("runAdd never returned:\n%s", out.String())
	}
}
