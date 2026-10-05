package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/itsgg/pagir/internal/platform"
	"github.com/itsgg/pagir/internal/record"
	"github.com/itsgg/pagir/internal/tailnet"
	"github.com/itsgg/pagir/internal/web"
)

const (
	publicHTTPS  = 443  // Funnel's port for public shares
	tailnetHTTPS = 8443 // tailnet-only shares; a funnel on 443 would make them public
	hubTick      = time.Second
	mountWait    = 60 * time.Second
	childBackoff = 3 * time.Second
	publicWatch  = 5 * time.Minute  // how long the hub checks a new share's public addresses
	attachedFor  = 10 * time.Second // a foreground share whose terminal has not touched it for this long is over
	bootSlack    = time.Minute      // boot time moves with the wall clock; small steps must not drop live shares
)

// The hub serves every share. tailscaled allows one foreground listener per
// port, so concurrent shares need one process that owns the port; the hub
// owns 443 through one foreground `tailscale funnel` and 8443 through one
// `tailscale serve`, both proxying to its own local listeners, and routes
// each request by the token in its first path segment. It starts with the
// first share and keeps the funnel up after the last, because Funnel's
// ingress nodes take up to two minutes to learn of a funnel that was off;
// `pagir stop hub` ends it. A crash still leaves nothing published.
type hub struct {
	host   string
	logger *log.Logger
	lanes  [2]*lane // public, tailnet

	mu     sync.RWMutex
	byTok  map[string]*live
	byID   map[string]*live
	failed map[string]string // id: why web.New refused it

	last     []byte    // hub.json as last written
	lastSync time.Time // when sync last ran, to notice the machine slept
}

type live struct {
	rec   *record.Share
	srv   *web.Server
	check *publicCheck // nil until the share's lane is mounted
}

// publicCheck follows one public share's addresses until all answer.
type publicCheck struct {
	cancel      context.CancelFunc
	started     time.Time
	took        time.Duration
	reached, of int
	done        bool
	err         string
}

// lane is one local listener and the tailscale child that publishes it.
type lane struct {
	public  bool
	https   int
	ln      net.Listener
	hs      *http.Server
	child   *exec.Cmd
	exited  chan error
	out     *tailBuffer
	started time.Time
	mounted bool
	fails   int    // child starts in a row that never mounted
	err     string // why the child last failed, until it mounts again
}

// laneFailures is how many starts in a row must fail before a lane's error
// reaches the shares on it. One exit is often transient (tailscaled
// restarting), and the hub retries it on its own.
const laneFailures = 3

func (l *lane) verb() string {
	if l.public {
		return "funnel"
	}
	return "serve"
}

func (l *lane) target() string {
	return fmt.Sprintf("http://127.0.0.1:%d", l.ln.Addr().(*net.TCPAddr).Port)
}

// cmdHub is the hub itself, run by the supervisor; stdout is hub.log.
func cmdHub() {
	logger := newLogger(os.Stdout)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := tailnet.GetStatus(ctx)
	if err != nil {
		logger.Fatal(err)
	}
	h := &hub{
		host:   st.Host(),
		logger: logger,
		byTok:  map[string]*live{},
		byID:   map[string]*live{},
		failed: map[string]string{},
	}
	for i, public := range []bool{true, false} {
		l := &lane{public: public, https: tailnetHTTPS}
		if public {
			l.https = publicHTTPS
		}
		if l.ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			logger.Fatal(err)
		}
		l.hs = &http.Server{Handler: h.route(public), ReadHeaderTimeout: 10 * time.Second, ErrorLog: logger}
		go l.hs.Serve(l.ln)
		h.lanes[i] = l
	}
	defer h.shutdown()

	logger.Printf("hub up for %s", h.host)
	parent := os.Getppid()
	tick := time.NewTicker(hubTick)
	defer tick.Stop()
	for {
		if hubStopAsked() {
			logger.Print("stopped")
			return
		}
		// Without its supervisor the lock is gone and a new hub may start,
		// so this one must go: Pdeathsig does it on Linux and a job object
		// on Windows; this check is what does it on macOS (on Windows the
		// parent pid never changes, so it never fires there).
		if os.Getppid() != parent {
			logger.Print("the supervisor is gone; stopping")
			return
		}
		h.sync(ctx)
		select {
		case <-ctx.Done():
			logger.Print("stopped")
			return
		case <-tick.C:
		}
	}
}

// route finds the share named by a request's first path segment. A share
// answers only on its own lane, so a tailnet-only token never works through
// Funnel.
func (h *hub) route(public bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		h.mu.RLock()
		s := h.byTok[tok]
		h.mu.RUnlock()
		if s == nil || s.rec.Public != public {
			http.NotFound(w, r)
			return
		}
		s.srv.ServeHTTP(w, r)
	})
}

// sync makes the hub serve exactly the shares the records ask for.
func (h *hub) sync(ctx context.Context) {
	recs, err := record.List()
	if err != nil {
		h.logger.Print(err)
	}
	now := time.Now()
	// After a sleep or a clock step the hub may run before a foreground
	// terminal has had its turn to touch its record; give it one tick. Wall
	// clock on purpose: the monotonic clock stops while the machine sleeps,
	// and the record's mtime, which Age reads, does not.
	wall := now.Round(0)
	slept := !h.lastSync.IsZero() && wall.Sub(h.lastSync) > attachedFor/2
	h.lastSync = wall
	boot, knowBoot := platform.BootTime()
	want := map[string]*record.Share{}
	for _, rec := range recs {
		switch {
		case knowBoot && rec.Created.Before(boot.Add(-bootSlack)):
			record.Remove(rec.ID)
			h.logger.Printf("%s dropped: made before the last reboot", rec.ID)
		case rec.Expired(now):
			record.Remove(rec.ID)
			h.logger.Printf("%s expired", rec.ID)
		case rec.Foreground && !slept && record.Age(rec.ID) > attachedFor:
			record.Remove(rec.ID)
			h.logger.Printf("%s ended with its terminal", rec.ID)
		default:
			want[rec.ID] = rec
		}
	}

	h.mu.Lock()
	for id, s := range h.byID {
		if w, ok := want[id]; !ok || w.Token != s.rec.Token {
			delete(h.byID, id)
			delete(h.byTok, s.rec.Token)
			s.stopCheck()
			s.srv.Close()
			h.logger.Printf("%s stopped", id)
		}
	}
	for id := range h.failed {
		if _, ok := want[id]; !ok {
			delete(h.failed, id)
		}
	}
	for id, rec := range want {
		if h.byID[id] != nil || h.failed[id] != "" {
			continue
		}
		srv, err := web.New(web.Config{
			Root:     rec.Path,
			Prefix:   rec.Mount(),
			Upload:   rec.Upload,
			Hidden:   rec.Hidden,
			Password: rec.Password,
			Log:      shareLogger(h.logger.Writer(), id),
			Unlogged: func(r *http.Request) bool { return r.UserAgent() == tailnet.ProbeAgent },
		})
		if err != nil {
			h.failed[id] = err.Error()
			h.logger.Printf("%s: %v", id, err)
			continue
		}
		s := &live{rec: rec, srv: srv}
		h.byID[id], h.byTok[rec.Token] = s, s
		h.logger.Printf("%s sharing %s", id, rec.Path)
	}
	// The public lane stays up with no shares, so the next share is public
	// at once instead of after the ingress nodes relearn the funnel.
	need := [2]bool{true, false}
	for _, s := range h.byID {
		need[lane0(s.rec.Public)] = true
	}
	h.mu.Unlock()

	for i, l := range h.lanes {
		h.tend(ctx, l, need[i])
	}
	h.checkPublic(ctx)
	h.publish(want)
}

// checkPublic starts a public check for each public share once its lane is
// mounted, drops the checks of a lane that lost its mount (a remount is cold
// again), and sends the notifications the CLI asked for once a check ends.
func (h *hub) checkPublic(ctx context.Context) {
	mounted := h.lanes[0].mounted
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, s := range h.byID {
		if !s.rec.Public {
			continue
		}
		switch {
		case !mounted:
			s.stopCheck()
		case s.check == nil:
			cctx, cancel := context.WithCancel(ctx)
			c := &publicCheck{cancel: cancel, started: time.Now()}
			s.check = c
			go h.runCheck(cctx, s.rec, c)
		case s.check.done && record.TakeNotify(id):
			go notify(s.rec, *s.check)
		}
	}
}

func (h *hub) runCheck(ctx context.Context, rec *record.Share, c *publicCheck) {
	reached, of, err := tailnet.WaitPublic(ctx, h.host, rec.URL, publicWatch, func(reached, of int) {
		h.mu.Lock()
		c.reached, c.of = reached, of
		h.mu.Unlock()
	})
	if ctx.Err() != nil {
		return // the share ended or its lane lost the mount
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c.reached, c.of, c.done = reached, of, true
	c.took = time.Since(c.started).Round(time.Second)
	if err != nil {
		c.err = err.Error()
		h.logger.Printf("%s public through %d of %d addresses after %s: %v", rec.ID, reached, of, c.took, err)
	}
}

func (s *live) stopCheck() {
	if s.check != nil {
		s.check.cancel()
		s.check = nil
	}
}

// notify tells the desktop how a public check the CLI stopped waiting for
// ended. A missing notifier is not an error: pagir ls shows the same.
func notify(rec *record.Share, c publicCheck) {
	name := filepath.Base(rec.Path)
	var body string
	switch {
	case c.err == "":
		body = fmt.Sprintf("%s now answers through all %d public addresses\n%s", name, c.of, rec.URL)
	case c.of == 0:
		body = fmt.Sprintf("%s is on your tailnet but not public: %s; pagir doctor may say why\n%s", name, c.err, rec.URL)
	default:
		body = fmt.Sprintf("%s answers through %d of %d public addresses after %s; pagir doctor may say why\n%s", name, c.reached, c.of, c.took, rec.URL)
	}
	platform.Notify("pagir", body)
}

func lane0(public bool) int {
	if public {
		return 0
	}
	return 1
}

// tend starts or stops a lane's tailscale child to match need, and notices
// when its mount appears or the child dies.
func (h *hub) tend(ctx context.Context, l *lane, need bool) {
	if l.child != nil {
		select {
		case err := <-l.exited:
			if !l.mounted {
				l.fails++
			}
			l.child, l.mounted = nil, false
			l.err = fmt.Sprintf("tailscale %s exited (%v): %s", l.verb(), err, l.out.String())
			h.logger.Print(l.err)
		default:
		}
	}
	switch {
	case need && l.child == nil:
		if time.Since(l.started) < childBackoff {
			return
		}
		l.started = time.Now()
		l.out = &tailBuffer{max: 2048}
		cmd := tailnet.MountCommand(l.public, l.https, l.target())
		w := io.MultiWriter(l.out, prefixWriter{h.logger, "tailscale: "})
		cmd.Stdout, cmd.Stderr = w, w
		cmd.WaitDelay = 2 * time.Second // a grandchild holding the pipe must not block Wait
		platform.TieToParent(cmd)
		if err := cmd.Start(); err != nil {
			l.fails = laneFailures // tailscale cannot even run; retrying will not help soon
			l.err = fmt.Sprintf("start tailscale: %v", err)
			h.logger.Print(l.err)
			return
		}
		platform.AfterStart(cmd)
		l.child, l.exited = cmd, make(chan error, 1)
		go func() { l.exited <- cmd.Wait() }()
	case !need:
		if l.child != nil {
			stopChild(l.child, l.exited)
			h.logger.Printf("tailscale %s on :%d stopped", l.verb(), l.https)
		}
		// A lane nobody needs forgets its failures, so a failed start or a
		// timeout cannot fail the next share before its child has a chance.
		l.child, l.mounted, l.fails, l.err = nil, false, 0, ""
		return
	}
	if l.child == nil || l.mounted {
		return
	}
	if sc, err := tailnet.GetServeConfig(ctx); err == nil && sc.HasMount(fmt.Sprintf("%s:%d", h.host, l.https), "/") {
		l.mounted, l.err, l.fails = true, "", 0
		h.logger.Printf("tailscale %s live on :%d", l.verb(), l.https)
	} else if time.Since(l.started) > mountWait {
		l.fails = laneFailures // a full minute without a mount is not transient
		l.err = fmt.Sprintf("tailscale %s did not mount within %s: %s", l.verb(), mountWait, l.out.String())
		h.logger.Print(l.err)
		stopChild(l.child, l.exited)
		l.child = nil
	}
}

// publish writes hub.json when anything in it changed.
func (h *hub) publish(want map[string]*record.Share) {
	hs := &record.Hub{
		PID:         os.Getpid(),
		PublicPort:  h.lanes[0].ln.Addr().(*net.TCPAddr).Port,
		TailnetPort: h.lanes[1].ln.Addr().(*net.TCPAddr).Port,
		Shares:      map[string]record.Status{},
	}
	h.mu.RLock()
	for id, rec := range want {
		if msg := h.failed[id]; msg != "" {
			hs.Shares[id] = record.Status{Error: msg}
			continue
		}
		l := h.lanes[lane0(rec.Public)]
		st := record.Status{Ready: l.mounted && h.byID[id] != nil}
		if !st.Ready && l.fails >= laneFailures {
			st.Error = l.err
		}
		if s := h.byID[id]; s != nil && s.check != nil {
			st.Reached, st.Of = s.check.reached, s.check.of
			st.Public = s.check.done && s.check.err == "" && s.check.of > 0
			st.PublicErr = s.check.err
		}
		hs.Shares[id] = st
	}
	h.mu.RUnlock()
	data, _ := json.Marshal(hs)
	if bytes.Equal(data, h.last) && record.HubWritten() {
		return
	}
	hs.Updated = time.Now()
	if err := record.SaveHub(hs); err != nil {
		h.logger.Print(err)
		return
	}
	h.last = data
}

// shutdown takes the mounts down first, then the listeners.
func (h *hub) shutdown() {
	for _, l := range h.lanes {
		if l != nil && l.child != nil {
			stopChild(l.child, l.exited)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, l := range h.lanes {
		if l != nil {
			l.hs.Shutdown(ctx)
		}
	}
	h.mu.Lock()
	for _, s := range h.byID {
		s.stopCheck()
		s.srv.Close()
	}
	h.mu.Unlock()
	record.RemoveHub(os.Getpid())
}

// stopChild ends a tailscale CLI, which makes tailscaled drop its mount.
func stopChild(cmd *exec.Cmd, exited chan error) {
	platform.Terminate(cmd.Process)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-exited
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(t.buf.String())
}

// prefixWriter logs each line written to it with a prefix.
type prefixWriter struct {
	logger *log.Logger
	prefix string
}

func (p prefixWriter) Write(b []byte) (int, error) {
	for line := range strings.Lines(string(b)) {
		if line = strings.TrimRight(line, "\r\n"); strings.TrimSpace(line) != "" {
			p.logger.Print(p.prefix + line)
		}
	}
	return len(b), nil
}
