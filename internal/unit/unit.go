// Package unit runs the hub as a transient systemd user unit, so systemd
// supervises, restarts and logs it.
package unit

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Start launches argv as a transient user service. Restart=on-failure brings
// it back after a crash; a clean exit ends it, and --collect removes the unit
// either way.
func Start(ctx context.Context, name, description string, env, argv []string) error {
	args := []string{
		"--user", "--quiet", "--collect",
		"--unit=" + name,
		"--description=" + description,
		"--property=Restart=on-failure",
		"--property=RestartSec=2",
	}
	for _, e := range env {
		args = append(args, "--setenv="+e)
	}
	args = append(args, "--")
	_, err := systemd(ctx, "systemd-run", append(args, argv...)...)
	return err
}

// Stop stops the unit; one that is already gone is not an error.
func Stop(ctx context.Context, name string) error {
	if _, err := systemd(ctx, "systemctl", "--user", "stop", name); err != nil && !strings.Contains(err.Error(), "not loaded") {
		return err
	}
	return nil
}

// State returns ActiveState and SubState, "inactive" and "dead" for a unit
// systemd no longer knows.
func State(ctx context.Context, name string) (active, sub string, err error) {
	out, err := systemd(ctx, "systemctl", "--user", "show", "--property=ActiveState,SubState", name)
	if err != nil {
		return "", "", err
	}
	for line := range strings.Lines(string(out)) {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "ActiveState":
			active = v
		case "SubState":
			sub = v
		}
	}
	return active, sub, nil
}

// Journal returns the unit's last n log lines.
func Journal(ctx context.Context, name string, n int) string {
	out, _ := exec.CommandContext(ctx, "journalctl", "--user", "--unit="+name, "--no-pager", "--output=cat", fmt.Sprintf("--lines=%d", n)).Output()
	return strings.TrimSpace(string(out))
}

func systemd(ctx context.Context, bin string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s: %s", bin, msg)
	}
	return out, nil
}
