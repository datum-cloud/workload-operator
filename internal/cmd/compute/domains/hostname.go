// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// maxHostnameLength is the length the API allows for a domain name.
const maxHostnameLength = 253

// hostnamePattern is the shape the platform stores a hostname in: lowercase
// RFC 1123 labels separated by dots. It matches the Domain API's own pattern,
// so a hostname this accepts is one the server will accept too.
var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// normalizeHostname reduces what a user might type or paste to the bare
// hostname the platform stores.
//
// People copy a domain out of a browser bar, out of a dig output, or out of
// this CLI's own output, so "https://API.Example.com/health", "api.example.com."
// and "api.example.com:443" all reach us meaning the same name. Comparisons —
// above all the one that refuses to remove the Datum-managed hostname — are
// made on this form, never on the raw string.
func normalizeHostname(raw string) string {
	h := strings.TrimSpace(raw)

	// A pasted URL: drop the scheme, then everything after the authority.
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+len("://"):]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	if i := strings.LastIndex(h, "@"); i >= 0 {
		h = h[i+1:]
	}
	// A port, but only when there is one colon to be sure it is a port: an
	// IPv6 literal has several, and must survive intact to be rejected as an
	// address rather than mangled into something that looks like a bad name.
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i >= 0 {
			h = h[1:i]
		}
	} else if i := strings.LastIndex(h, ":"); i >= 0 && isDigits(h[i+1:]) && !strings.Contains(h[:i], ":") {
		h = h[:i]
	}

	// A trailing dot is the same name, spelled absolutely.
	h = strings.TrimRight(h, ".")

	return strings.ToLower(h)
}

// sameHostname reports whether two hostnames name the same host, whatever form
// each was written in. An empty hostname matches nothing, so an unassigned
// managed hostname never swallows a user's argument.
func sameHostname(a, b string) bool {
	na, nb := normalizeHostname(a), normalizeHostname(b)
	return na != "" && na == nb
}

// validateHostname normalizes a hostname and reports why it cannot be used, in
// terms of what the user typed rather than the field the API would complain
// about.
func validateHostname(raw string) (string, error) {
	h := normalizeHostname(raw)

	switch {
	case h == "":
		return "", fmt.Errorf("no hostname given — pass the hostname to attach, for example api.example.com")
	case strings.Contains(h, "*"):
		return "", fmt.Errorf("wildcard hostnames are not supported — attach each hostname you want to serve")
	case len(h) > maxHostnameLength:
		return "", fmt.Errorf("hostname %q is longer than %d characters", h, maxHostnameLength)
	case net.ParseIP(h) != nil:
		return "", fmt.Errorf("%q is an IP address — a domain must be a hostname, for example api.example.com", h)
	case !strings.Contains(h, "."):
		return "", fmt.Errorf("%q is not a fully qualified hostname — use something like api.example.com", h)
	case !hostnamePattern.MatchString(h):
		return "", fmt.Errorf("%q is not a valid hostname — use letters, digits, dashes and dots", h)
	}

	return h, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
