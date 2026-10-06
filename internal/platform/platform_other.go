//go:build unix && !linux && !darwin

package platform

import (
	"os"
	"os/exec"
)

var tailscaleFallbacks = []string{"/usr/local/bin/tailscale"}

// TieToParent has no portable form here; the supervisor's KillGroup covers it.
func TieToParent(cmd *exec.Cmd) {}

func clipboard() []string {
	if os.Getenv("DISPLAY") != "" {
		return []string{"xclip", "-selection", "clipboard"}
	}
	return nil
}

// Notify shows a desktop notification through notify-send, best effort.
func Notify(title, body string) error {
	return exec.Command("notify-send", "--app-name=pagir", title, body).Run()
}
