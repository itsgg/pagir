package platform

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var tailscaleFallbacks = []string{`C:\Program Files\Tailscale\tailscale.exe`}

// Detach starts cmd with no console, in its own process group.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

// Lead is a no-op on Windows: children are tied to the hub by a job object.
func Lead(cmd *exec.Cmd) {}

// KillGroup is a no-op on Windows; the job object has already ended them.
func KillGroup(pid int) {}

// TieToParent is done after start on Windows, by AfterStart.
func TieToParent(cmd *exec.Cmd) {}

var (
	jobOnce sync.Once
	job     windows.Handle
)

// AfterStart puts cmd in a job object that kills its processes when this
// process exits, however it ends: Windows' form of Pdeathsig.
func AfterStart(cmd *exec.Cmd) {
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			windows.CloseHandle(h)
			return
		}
		job = h
	})
	if job == 0 || cmd.Process == nil {
		return
	}
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(p)
	windows.AssignProcessToJobObject(job, p)
}

// TryLock takes an exclusive lock on path without waiting. The lock goes
// when the process does, however it ends.
func TryLock(path string) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
}

// Unroutable reports a dial that cannot leave this machine at all.
func Unroutable(err error) bool {
	return errors.Is(err, windows.WSAENETUNREACH) || errors.Is(err, windows.WSAEHOSTUNREACH) || errors.Is(err, windows.WSAEADDRNOTAVAIL)
}

func bootTime() (time.Time, error) {
	return time.Now().Add(-windows.DurationSinceBoot()).Truncate(time.Second), nil
}

func clipboard() []string { return []string{"clip"} }

// Notify shows a balloon notification through PowerShell, best effort.
func Notify(title, body string) error {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := `Add-Type -AssemblyName System.Windows.Forms; $n = New-Object System.Windows.Forms.NotifyIcon; ` +
		`$n.Icon = [System.Drawing.SystemIcons]::Information; $n.Visible = $true; ` +
		`$n.ShowBalloonTip(10000, ` + q(title) + `, ` + q(body) + `, 'Info'); Start-Sleep -Seconds 11; $n.Dispose()`
	return exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", script).Start()
}

// Terminate ends a process. Windows cannot deliver SIGTERM, and the
// tailscale CLI needs no graceful exit: tailscaled drops its mount when the
// connection closes.
func Terminate(p *os.Process) error { return p.Kill() }
