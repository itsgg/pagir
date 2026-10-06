package platform

import (
	"os"
	"os/exec"
	"syscall"
)

var tailscaleFallbacks = []string{"/usr/bin/tailscale", "/usr/local/bin/tailscale"}

// TieToParent makes cmd die with this process, even one killed outright.
func TieToParent(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}

func clipboard() []string {
	switch {
	case os.Getenv("WAYLAND_DISPLAY") != "":
		return []string{"wl-copy"}
	case os.Getenv("DISPLAY") != "":
		if _, err := exec.LookPath("xclip"); err == nil {
			return []string{"xclip", "-selection", "clipboard"}
		}
		return []string{"xsel", "--clipboard", "--input"}
	}
	return nil
}

// Notify shows a desktop notification through notify-send, best effort.
func Notify(title, body string) error {
	return exec.Command("notify-send", "--app-name=pagir", title, body).Run()
}
