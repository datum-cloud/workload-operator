// SPDX-License-Identifier: AGPL-3.0-only

package open

import (
	"context"
	"strings"
	"testing"
)

// The substrings the refusal tests match on, named once so the same wording is
// asserted here and in open_test.go.
const (
	wantNotHTTPS    = "only https:// URLs"
	wantCredentials = "embedded credentials"
	wantMalformed   = "not a valid URL"
)

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		wantErr string
	}{
		{name: "managed hostname", rawURL: "https://a1b2c3d4.datumproxy.net"},
		{name: "custom hostname with a path", rawURL: "https://api.example.com/healthz"},
		{name: "explicit port", rawURL: "https://api.example.com:8443"},

		{name: "empty", rawURL: "", wantErr: "no URL to open"},
		{name: "plaintext http", rawURL: "http://api.example.com", wantErr: wantNotHTTPS},
		{name: "javascript scheme", rawURL: "javascript:alert(1)", wantErr: wantNotHTTPS},
		{name: "file scheme", rawURL: "file:///etc/passwd", wantErr: wantNotHTTPS},
		{name: "scheme-relative", rawURL: "//api.example.com", wantErr: wantNotHTTPS},
		{name: "bare hostname", rawURL: "api.example.com", wantErr: wantNotHTTPS},
		{name: "looks like a flag", rawURL: "--version", wantErr: wantNotHTTPS},
		{name: "no host", rawURL: "https://", wantErr: "no host"},
		{name: "embedded credentials", rawURL: "https://user:pw@api.example.com", wantErr: wantCredentials},
		{name: "leading whitespace", rawURL: " https://api.example.com", wantErr: "whitespace"},
		{name: "trailing newline", rawURL: "https://api.example.com\n", wantErr: "whitespace"},
		{name: "unparseable", rawURL: "https://not a host", wantErr: wantMalformed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateURL(tc.rawURL)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validateURL(%q) = %v, want nil", tc.rawURL, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validateURL(%q) = nil, want an error containing %q", tc.rawURL, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("validateURL(%q) = %q, want it to contain %q", tc.rawURL, err, tc.wantErr)
			}
		})
	}
}

// The real opener refuses anything validateURL refuses, before a process is
// ever started.
func TestOpenInDefaultBrowserValidatesFirst(t *testing.T) {
	if err := openInDefaultBrowser(context.Background(), "http://api.example.com"); err == nil {
		t.Fatal("openInDefaultBrowser opened a plaintext URL, want a refusal")
	}
}

func TestBrowserCommand(t *testing.T) {
	const rawURL = "https://a1b2c3d4.datumproxy.net"

	tests := []struct {
		goos     string
		wantName string
		wantArgs []string
	}{
		{goos: "darwin", wantName: "open", wantArgs: []string{rawURL}},
		{goos: "linux", wantName: xdgOpen, wantArgs: []string{rawURL}},
		{goos: "freebsd", wantName: xdgOpen, wantArgs: []string{rawURL}},
		{goos: "windows", wantName: "rundll32", wantArgs: []string{"url.dll,FileProtocolHandler", rawURL}},
	}

	for _, tc := range tests {
		t.Run(tc.goos, func(t *testing.T) {
			name, args := browserCommand(tc.goos, rawURL)
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if strings.Join(args, " ") != strings.Join(tc.wantArgs, " ") {
				t.Errorf("args = %q, want %q", args, tc.wantArgs)
			}
		})
	}
}
