// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.datum.net/compute/internal/cmd/compute/url"
)

// TestRemoveRefusesTheManagedDomain is the rule the spec states outright: the
// Datum-managed URL always appears in the list and cannot be removed. It has to
// hold however the user spells it, because the spelling they have to hand is
// whatever they copied out of a browser or out of this CLI's own output.
func TestRemoveRefusesTheManagedDomain(t *testing.T) {
	variants := []struct {
		name  string
		given string
	}{
		{"exactly", testCanonical},
		{"upper case", "A1B2C3D4.DATUMPROXY.NET"},
		{"mixed case", "A1b2C3d4.DatumProxy.Net"},
		{"with a trailing dot", testCanonical + "."},
		{"https prefix", "https://" + testCanonical},
		{"https prefix and trailing slash", "https://" + testCanonical + "/"},
		{"https prefix and a path", "https://" + testCanonical + "/healthz"},
		{"https prefix, upper case and trailing dot", "HTTPS://A1B2C3D4.DatumProxy.NET./"},
		{"surrounding whitespace", "  " + testCanonical + "  "},
		{"with a port", testCanonical + ":443"},
	}

	for _, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClient(t,
				programmedProxy(testWorkload, testCanonical, testCustom),
				serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)),
			)

			var out bytes.Buffer
			err := runRemove(context.Background(), &out, c, testProject, testWorkload, tc.given, true)
			if err == nil {
				t.Fatalf("removing the managed domain as %q was allowed", tc.given)
			}
			if !strings.Contains(err.Error(), "cannot be removed") {
				t.Errorf("error = %q, want it to say the domain cannot be removed", err)
			}

			// And it is still there.
			if got := hostnamesOf(t, c); len(got) != 1 || got[0] != testCustom {
				t.Errorf("hostnames = %v, want the custom one untouched", got)
			}
		})
	}
}

// The managed hostname is refused even when the platform reported it only in
// the per-hostname list and not in the canonical field.
func TestRemoveRefusesManagedHostnameFromInfo(t *testing.T) {
	info := &url.Info{
		Hostnames: []url.Hostname{{Hostname: testCanonical, Managed: true}},
	}
	if !managed(info, "https://"+testCanonical+"/") {
		t.Error("a hostname marked managed should be refused")
	}
	if managed(info, testCustom) {
		t.Error("a custom hostname should not be treated as managed")
	}
}

func TestRemoveDetachesHostname(t *testing.T) {
	c := newFakeClient(t,
		programmedProxy(testWorkload, testCanonical, testCustom, "www.example.com"),
		serviceWith(testWorkload, 8080, location("DFW", 1, 1, true)),
	)

	var out bytes.Buffer
	if err := runRemove(context.Background(), &out, c, testProject, testWorkload, "HTTPS://API.Example.com./", true); err != nil {
		t.Fatalf("runRemove: %v", err)
	}

	if got := hostnamesOf(t, c); len(got) != 1 || got[0] != "www.example.com" {
		t.Errorf("hostnames = %v, want only www.example.com left", got)
	}

	got := out.String()
	if !strings.Contains(got, testCustom+" removed from workload") {
		t.Errorf("missing the confirmation line:\n%s", got)
	}
	if !strings.Contains(got, "Still serving on https://"+testCanonical) {
		t.Errorf("should say what the workload still serves on:\n%s", got)
	}
	// Per the spec, a verified domain outlives the workload it was attached to.
	if !strings.Contains(got, "stays verified in this project") {
		t.Errorf("should say the domain itself is kept:\n%s", got)
	}
	if !strings.Contains(got, "datumctl compute domains add "+testWorkload+" "+testCustom) {
		t.Errorf("should say how to attach it again:\n%s", got)
	}
}

func TestRemoveErrors(t *testing.T) {
	tests := []struct {
		name     string
		objects  []client.Object
		workload string
		hostname string
		yes      bool
		want     string
	}{
		{
			name:     "hostname is not attached",
			objects:  []client.Object{programmedProxy(testWorkload, testCanonical, testCustom)},
			workload: testWorkload,
			hostname: "other.example.com",
			yes:      true,
			want:     "is not attached to workload",
		},
		{
			name:     "workload has no custom domains at all",
			objects:  []client.Object{programmedProxy(testWorkload, testCanonical)},
			workload: testWorkload,
			hostname: testCustom,
			yes:      true,
			want:     "has no custom domains",
		},
		{
			name:     "workload has no URL",
			objects:  []client.Object{workload(testWorkload)},
			workload: testWorkload,
			hostname: testCustom,
			yes:      true,
			want:     wantPublishHint,
		},
		{
			name:     "hostname argument is blank",
			objects:  []client.Object{programmedProxy(testWorkload, testCanonical, testCustom)},
			workload: testWorkload,
			hostname: "   ",
			yes:      true,
			want:     wantNoHostname,
		},
		{
			// go test runs without a terminal, which is the same position a CI
			// job is in: say so rather than removing a domain unasked.
			name:     "no terminal to confirm on and no --yes",
			objects:  []client.Object{programmedProxy(testWorkload, testCanonical, testCustom)},
			workload: testWorkload,
			hostname: testCustom,
			yes:      false,
			want:     "--yes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClient(t, tc.objects...)
			var out bytes.Buffer
			err := runRemove(context.Background(), &out, c, testProject, tc.workload, tc.hostname, tc.yes)
			if err == nil {
				t.Fatalf("expected an error, got output:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRemoveLeavesTheProxyWhenRefusing(t *testing.T) {
	c := newFakeClient(t, programmedProxy(testWorkload, testCanonical, testCustom))

	var out bytes.Buffer
	if err := runRemove(context.Background(), &out, c, testProject, testWorkload, "other.example.com", true); err == nil {
		t.Fatal("expected an error")
	}
	if got := hostnamesOf(t, c); len(got) != 1 || got[0] != testCustom {
		t.Errorf("hostnames = %v, want them untouched", got)
	}
}

func TestIndexOfNormalizes(t *testing.T) {
	hostnames := hostnameList("www.example.com", "API.Example.com.")

	tests := []struct {
		name  string
		given string
		want  int
	}{
		{"exact", "www.example.com", 0},
		{"stored in another spelling", testCustom, 1},
		{"pasted as a URL", "https://api.example.com/", 1},
		{"absent", "nope.example.com", -1},
		{"nothing at all", "", -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := indexOf(hostnames, tc.given); got != tc.want {
				t.Errorf("indexOf(%q) = %d, want %d", tc.given, got, tc.want)
			}
		})
	}
}
