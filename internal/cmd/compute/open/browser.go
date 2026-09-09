// SPDX-License-Identifier: AGPL-3.0-only

package open

import (
	"context"
	"fmt"
	neturl "net/url"
	"os/exec"
	"runtime"
	"strings"
)

// xdgOpen is the URL handler on every platform that is not macOS or Windows.
const xdgOpen = "xdg-open"

// opener launches a URL in the user's default browser. It is a function type
// so tests can substitute one that records the URL instead of opening it.
type opener func(ctx context.Context, rawURL string) error

// openBrowser is the opener used in production. Tests pass their own to run
// rather than reassigning this, so no test can start a real browser.
var openBrowser opener = openInDefaultBrowser

// openInDefaultBrowser hands a URL to the platform's URL handler.
//
// The URL is validated before it gets here, so nothing but an https:// URL the
// server produced is ever passed on. No shell is involved: the argument is
// passed to the handler directly and is never word-split or expanded.
func openInDefaultBrowser(ctx context.Context, rawURL string) error {
	if err := validateURL(rawURL); err != nil {
		return err
	}

	name, args := browserCommand(runtime.GOOS, rawURL)

	cmd := exec.CommandContext(ctx, name, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("opening browser with %s: %w", name, err)
	}
	return nil
}

// browserCommand returns the URL handler for an operating system and the
// arguments to hand it. Split out from openInDefaultBrowser so it can be
// tested for every platform without launching anything.
func browserCommand(goos, rawURL string) (name string, args []string) {
	switch goos {
	case "darwin":
		return "open", []string{rawURL}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		return xdgOpen, []string{rawURL}
	}
}

// validateURL rejects anything that is not an https:// URL of the shape the
// platform hands back. The CLI only ever opens a URL the server reported, and
// this is the check that keeps it that way.
func validateURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("no URL to open")
	}
	if strings.TrimSpace(rawURL) != rawURL {
		return fmt.Errorf("refusing to open %q: URL has leading or trailing whitespace", rawURL)
	}

	u, err := neturl.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("refusing to open %q: not a valid URL", rawURL)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("refusing to open %q: only https:// URLs are opened", rawURL)
	}
	if u.Host == "" {
		return fmt.Errorf("refusing to open %q: URL has no host", rawURL)
	}
	if u.User != nil {
		return fmt.Errorf("refusing to open %q: URL carries embedded credentials", rawURL)
	}
	return nil
}
