// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"strings"
	"testing"
)

func TestNormalizeHostname(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already normal", testCustom, testCustom},
		{"upper case", "API.Example.COM", testCustom},
		{"one trailing dot", testCustom + ".", testCustom},
		{"several trailing dots", testCustom + "..", testCustom},
		{"surrounding space", "  " + testCustom + " \n", testCustom},
		{"https prefix", "https://" + testCustom, testCustom},
		{"http prefix", "http://" + testCustom, testCustom},
		{"url with path", "https://" + testCustom + "/healthz", testCustom},
		{"url with query", "https://" + testCustom + "?x=1", testCustom},
		{"url with fragment", "https://" + testCustom + "#top", testCustom},
		{"url with port", "https://" + testCustom + ":443/", testCustom},
		{"bare host and port", testCustom + ":8443", testCustom},
		{"credentials", "https://user:pw@" + testCustom + "/", testCustom},
		{"everything at once", "  HTTPS://API.Example.com.:443/health?x=1  ", testCustom},
		{"nothing at all", "", ""},
		{"only a dot", ".", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeHostname(tc.in); got != tc.want {
				t.Errorf("normalizeHostname(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSameHostname(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", testCanonical, testCanonical, true},
		{"case differs", "A1B2C3D4.DatumProxy.NET", testCanonical, true},
		{"one trailing dot", testCanonical + ".", testCanonical, true},
		{"scheme pasted", "https://" + testCanonical, testCanonical, true},
		{"scheme and slash pasted", "https://" + testCanonical + "/", testCanonical, true},
		{"different hosts", testCustom, testCanonical, false},
		{"one empty", "", testCanonical, false},
		{"both empty", "", "", false},
		{"suffix is not the same host", "evil-" + testCanonical, testCanonical, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameHostname(tc.a, tc.b); got != tc.want {
				t.Errorf("sameHostname(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestValidateHostname(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "plain", in: testCustom, want: testCustom},
		{name: "normalized on the way through", in: "HTTPS://API.Example.com./", want: testCustom},
		{name: "only whitespace", in: "  ", wantErr: wantNoHostname},
		{name: "wildcard", in: "*.example.com", wantErr: "wildcard hostnames are not supported"},
		{name: "ipv4", in: "192.0.2.10", wantErr: "is an IP address"},
		{name: "ipv6", in: "2001:db8::1", wantErr: "is an IP address"},
		{name: "single label", in: "localhost", wantErr: "not a fully qualified hostname"},
		{name: "underscore", in: "api_v2.example.com", wantErr: "not a valid hostname"},
		{name: "leading dash label", in: "-api.example.com", wantErr: "not a valid hostname"},
		{name: "too long", in: strings.Repeat("a", 250) + ".example.com", wantErr: "longer than 253"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateHostname(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("validateHostname(%q) = %q, want an error", tc.in, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateHostname(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("validateHostname(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
