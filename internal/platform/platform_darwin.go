package platform

import (
	"os/exec"
	"strconv"
)

// The Mac App Store and standalone apps ship the CLI inside the bundle.
var tailscaleFallbacks = []string{
	"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
	"/opt/homebrew/bin/tailscale",
	"/usr/local/bin/tailscale",
}

// TieToParent has no macOS equivalent of Pdeathsig; the supervisor's
// KillGroup ends a child its hub leaves behind.
func TieToParent(cmd *exec.Cmd) {}

func clipboard() []string { return []string{"pbcopy"} }

// Notify shows a notification through osascript, best effort.
func Notify(title, body string) error {
	script := "display notification " + strconv.Quote(body) + " with title " + strconv.Quote(title)
	return exec.Command("osascript", "-e", script).Run()
}
