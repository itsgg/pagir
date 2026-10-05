package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/itsgg/pagir/internal/platform"
	"github.com/itsgg/pagir/internal/record"
)

// The supervisor is what the CLI starts, detached. It holds hub.lock for as
// long as a hub runs, which is how the CLI knows one does; it writes
// hub.log; and it runs the hub as a child it restarts after a crash, ending
// whatever tailscale processes a dead child left behind. It plays the part
// systemd played before pagir ran on macOS and Windows.

const (
	logLimit      = 5 << 20 // hub.log moves to hub.log.1 past this, at each start
	restartLimit  = 5       // crashes within restartWindow before giving up
	restartWindow = time.Minute
)

func cmdSupervise() {
	lockPath, err := record.Path("hub.lock")
	if err != nil {
		log.Fatal(err)
	}
	// A CLI checking whether a hub runs holds the lock for microseconds, so
	// one refusal does not yet mean another supervisor has it.
	var unlock func()
	for range 10 {
		var ok bool
		if unlock, ok, err = platform.TryLock(lockPath); err != nil {
			log.Fatal(err)
		}
		if ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if unlock == nil {
		return // a hub already runs
	}
	defer unlock()
	if stop, err := record.Path("hub.stop"); err == nil {
		os.Remove(stop) // left by a hub that died before reading it
	}
	logFile, err := openLog()
	if err != nil {
		log.Fatal(err)
	}
	defer logFile.Close()
	logger := newLogger(logFile)
	exe, err := os.Executable()
	if err != nil {
		logger.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var crashes []time.Time
	for {
		cmd := exec.Command(exe, "hub", "--child")
		cmd.Stdout, cmd.Stderr = logFile, logFile
		platform.Lead(cmd)
		platform.TieToParent(cmd) // the hub must not outlive the lock that says it runs
		if err := cmd.Start(); err != nil {
			logger.Print(err)
			return
		}
		platform.AfterStart(cmd)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var err error
		select {
		case err = <-done:
		case <-ctx.Done():
			// Stop the child the way the CLI does, which works everywhere.
			askHubToStop()
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				err = <-done
			}
		}
		platform.KillGroup(cmd.Process.Pid)
		record.RemoveHub(cmd.Process.Pid) // a dead hub's status must not read as live
		if err == nil || ctx.Err() != nil {
			return
		}
		now := time.Now()
		recent := crashes[:0]
		for _, t := range crashes {
			if now.Sub(t) < restartWindow {
				recent = append(recent, t)
			}
		}
		crashes = append(recent, now)
		if len(crashes) >= restartLimit {
			logger.Printf("the hub crashed %d times within %s; giving up", len(crashes), restartWindow)
			return
		}
		logger.Printf("the hub exited (%v); restarting", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// askHubToStop leaves hub.stop, which the hub reads on its next tick.
func askHubToStop() error {
	p, err := record.Path("hub.stop")
	if err != nil {
		return err
	}
	return os.WriteFile(p, nil, 0o600)
}

// hubStopAsked takes hub.stop and reports whether it was there.
func hubStopAsked() bool {
	p, err := record.Path("hub.stop")
	return err == nil && os.Remove(p) == nil
}

func openLog() (*os.File, error) {
	p, err := record.Path("hub.log")
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(p); err == nil && fi.Size() > logLimit {
		os.Rename(p, p+".1")
	}
	return os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
}

// logStamp is how every hub.log line starts; shareLines parses it back.
const logStamp = "2006/01/02 15:04:05.000000"

func newLogger(w io.Writer) *log.Logger {
	return log.New(w, "", log.Ldate|log.Ltime|log.Lmicroseconds)
}

// shareLogger writes one share's lines: the stamp, then its id, which is
// what pagir log and a foreground share look for.
func shareLogger(w io.Writer, id string) *log.Logger {
	return log.New(w, id+" ", log.Ldate|log.Ltime|log.Lmicroseconds|log.Lmsgprefix)
}

// hubRunning reports whether a supervisor holds hub.lock.
func hubRunning() bool {
	p, err := record.Path("hub.lock")
	if err != nil {
		return false
	}
	unlock, ok, err := platform.TryLock(p)
	if err != nil {
		return false
	}
	if ok {
		unlock()
		return false
	}
	return true
}

// startHub launches the supervisor detached from this terminal.
func startHub() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "hub")
	platform.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return errors.New("start the hub: " + err.Error())
	}
	return cmd.Process.Release()
}
