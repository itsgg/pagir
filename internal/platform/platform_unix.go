//go:build unix

package platform

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Detach starts cmd in its own session, so it outlives the terminal.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// Lead makes cmd lead a process group its children join, so KillGroup
// reaches whatever it leaves behind.
func Lead(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillGroup ends the rest of the group pid led.
func KillGroup(pid int) {
	syscall.Kill(-pid, syscall.SIGTERM)
}

// AfterStart is a no-op on Unix; the group and, on Linux, Pdeathsig do the work.
func AfterStart(cmd *exec.Cmd) {}

// TryLock takes an exclusive lock on path without waiting. The lock goes
// when the process does, however it ends.
func TryLock(path string) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
}

// Unroutable reports a dial that cannot leave this machine at all.
func Unroutable(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.EADDRNOTAVAIL)
}

// Terminate asks a process to end.
func Terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
