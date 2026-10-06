// Package platform holds what differs between Linux, macOS and Windows:
// starting the hub detached, tying tailscale children to it, locking, the
// clipboard, notifications and finding tailscale.
package platform

import (
	"os"
	"os/exec"
	"strings"
)

// Copy puts text on the clipboard and reports whether one took it.
func Copy(text string) bool {
	argv := clipboard()
	if argv == nil {
		return false
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run() == nil
}

// Clipboard names the copy command for this session, "" without one.
func Clipboard() string {
	if argv := clipboard(); argv != nil {
		return argv[0]
	}
	return ""
}

// Tailscale is the tailscale CLI: the one on PATH, else where the installer
// for this system puts it.
func Tailscale() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	for _, p := range tailscaleFallbacks {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "tailscale"
}
