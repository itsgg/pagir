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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/itsgg/pagir/internal/record"
	"github.com/itsgg/pagir/internal/tailnet"
	"github.com/itsgg/pagir/internal/web"
)

const (
	hubUnit      = "pagir.service"
	publicHTTPS  = 443  // Funnel's port for public shares
	tailnetHTTPS = 8443 // tailnet-only shares; a funnel on 443 would make them public
	hubTick      = time.Second
	hubIdle      = 10 * time.Second // exit once no share has been wanted for this long
	mountWait    = 60 * time.Second
	childBackoff = 3 * time.Second
)

// The hub serves every share. tailscaled allows one foreground listener per
// port, so concurrent shares need one process that owns the port; the hub
// owns 443 through one foreground `tailscale funnel` and 8443 through one
// `tailscale serve`, both proxying to its own local listeners, and routes
// each request by the token in its first path segment. It starts with the
// first share and exits after the last, so nothing listens on 443 while
// nothing is shared, and a crash still leaves nothing published.
type hub struct {
	host   string
	boot   string
	logger *log.Logger
	lanes  [2]*lane // public, tailnet

	mu     sync.RWMutex
	byTok  map[string]*live
	byID   map[string]*live
	failed map[string]string // id: why web.New refused it

	last []byte // hub.json as last written
}

type live struct {
	rec *record.Share
	srv *web.Server
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

func cmdHub() {
	logger := log.New(os.Stderr, "", 0) // the journal adds the time
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := tailnet.GetStatus(ctx)
	if err != nil {
		logger.Fatal(err)
	}
	h := &hub{
		host:   st.Host(),
		boot:   record.CurrentBoot(),
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
	idle := time.Now()
	tick := time.NewTicker(hubTick)
	defer tick.Stop()
	for {
		if h.sync(ctx) > 0 {
			idle = time.Now()
		} else if time.Since(idle) > hubIdle {
			logger.Print("no shares left")
			return
		}
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

// sync makes the hub serve exactly the shares the records ask for, and
// returns how many that is.
func (h *hub) sync(ctx context.Context) int {
	recs, err := record.List()
	if err != nil {
		h.logger.Print(err)
	}
	now := time.Now()
	want := map[string]*record.Share{}
	for _, rec := range recs {
		switch {
		case rec.Boot != h.boot:
			record.Remove(rec.ID)
			h.logger.Printf("%s dropped: made before the last reboot", rec.ID)
		case rec.Expired(now):
			record.Remove(rec.ID)
			h.logger.Printf("%s expired", rec.ID)
		case rec.Foreground && !processIsPagir(rec.Owner):
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
			Log:      log.New(h.logger.Writer(), id+" ", 0),
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
	var need [2]bool
	for _, s := range h.byID {
		need[lane0(s.rec.Public)] = true
	}
	h.mu.Unlock()

	for i, l := range h.lanes {
		h.tend(ctx, l, need[i])
	}
	h.publish(want)
	return len(want)
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
		if err := cmd.Start(); err != nil {
			l.fails = laneFailures // tailscale cannot even run; retrying will not help soon
			l.err = fmt.Sprintf("start tailscale: %v", err)
			h.logger.Print(l.err)
			return
		}
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
		hs.Shares[id] = st
	}
	h.mu.RUnlock()
	data, _ := json.Marshal(hs)
	if bytes.Equal(data, h.last) {
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
		s.srv.Close()
	}
	h.mu.Unlock()
	record.RemoveHub()
}

// stopChild ends a tailscale CLI, which makes tailscaled drop its mount.
func stopChild(cmd *exec.Cmd, exited chan error) {
	cmd.Process.Signal(syscall.SIGTERM)
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
