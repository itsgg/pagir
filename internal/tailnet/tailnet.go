// Package tailnet talks to tailscaled through the tailscale CLI. The CLI on
// the machine always matches its daemon; a Go client library older than the
// daemon would drop serve-config fields it does not know when writing the
// config back.
package tailnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Status is the part of `tailscale status --json` pagir reads.
type Status struct {
	BackendState string
	Health       []string
	Self         struct {
		DNSName string
		Online  bool
		CapMap  map[string]json.RawMessage
	}
}

// Host is this node's MagicDNS name without the trailing dot.
func (s *Status) Host() string { return strings.TrimSuffix(s.Self.DNSName, ".") }

// Can reports whether this node holds a capability such as "funnel" or "https".
func (s *Status) Can(capability string) bool {
	_, ok := s.Self.CapMap[capability]
	return ok
}

// GetStatus runs `tailscale status --json`.
func GetStatus(ctx context.Context) (*Status, error) {
	out, err := run(ctx, "status", "--json")
	if err != nil {
		return nil, err
	}
	return ParseStatus(out)
}

// ParseStatus parses `tailscale status --json` output.
func ParseStatus(data []byte) (*Status, error) {
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	return &s, nil
}

// ServeConfig is the part of `tailscale serve status --json` pagir reads.
// Foreground holds one config per running foreground CLI session.
type ServeConfig struct {
	Web        map[string]WebConfig    `json:",omitempty"`
	Foreground map[string]*ServeConfig `json:",omitempty"`
}

// WebConfig is the handlers of one host:port.
type WebConfig struct {
	Handlers map[string]Handler
}

// Handler is one mount point.
type Handler struct {
	Path  string `json:",omitempty"`
	Proxy string `json:",omitempty"`
	Text  string `json:",omitempty"`
}

// GetServeConfig runs `tailscale serve status --json`, which shows the
// foreground sessions that `tailscale funnel status` leaves out.
func GetServeConfig(ctx context.Context) (*ServeConfig, error) {
	out, err := run(ctx, "serve", "status", "--json")
	if err != nil {
		return nil, err
	}
	return ParseServeConfig(out)
}

// ParseServeConfig parses `tailscale serve status --json`; no config at all
// comes back as empty output or {}.
func ParseServeConfig(data []byte) (*ServeConfig, error) {
	var c ServeConfig
	if len(bytes.TrimSpace(data)) == 0 {
		return &c, nil
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("tailscale serve status: %w", err)
	}
	return &c, nil
}

// each visits every web config, foreground sessions included.
func (c *ServeConfig) each(f func(hostPort string, w WebConfig)) {
	for hp, w := range c.Web {
		f(hp, w)
	}
	for _, fg := range c.Foreground {
		if fg != nil {
			fg.each(f)
		}
	}
}

// HasMount reports whether hostPort (host:443) serves mount, in any session.
func (c *ServeConfig) HasMount(hostPort, mount string) bool {
	found := false
	c.each(func(hp string, w WebConfig) {
		if hp == hostPort {
			if _, ok := w.Handlers[mount]; ok {
				found = true
			}
		}
	})
	return found
}

// Foreign lists the handlers on hostPort that do not proxy to ours, the
// hub's local address. A foreground listener owns its whole port, so any of
// them keeps the hub from publishing there.
func (c *ServeConfig) Foreign(hostPort, ours string) []string {
	var out []string
	c.each(func(hp string, w WebConfig) {
		if hp != hostPort {
			return
		}
		for mount, h := range w.Handlers {
			if h.Proxy != ours {
				target := h.Proxy + h.Path + h.Text
				out = append(out, fmt.Sprintf("https://%s%s -> %s", hp, mount, target))
			}
		}
	})
	return out
}

// PathHandlers lists the directories and files tailscaled serves itself.
// While any exists, tailscaled refuses serve changes from a user who is not
// root and cannot run sudo, because it checks the whole config.
func (c *ServeConfig) PathHandlers() []string {
	var out []string
	c.each(func(hp string, w WebConfig) {
		for mount, h := range w.Handlers {
			if h.Path != "" {
				out = append(out, fmt.Sprintf("https://%s%s -> %s", strings.TrimSuffix(hp, ":443"), mount, h.Path))
			}
		}
	})
	return out
}

// MountCommand returns the foreground `tailscale funnel` (or `serve`, for
// the tailnet only) that publishes all of httpsPort by proxying to target.
// The mount lives exactly as long as this process: tailscaled drops a
// foreground mount when the CLI's connection closes, however the CLI ends.
func MountCommand(public bool, httpsPort int, target string) *exec.Cmd {
	verb := "serve"
	if public {
		verb = "funnel"
	}
	cmd := exec.Command("tailscale", verb, fmt.Sprintf("--https=%d", httpsPort), target)
	// Its own process group keeps a terminal's Ctrl+C away from it, so the
	// hub decides when it stops; Pdeathsig takes it down if the hub is killed.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	return cmd
}

// Operator returns the user tailscaled lets change its config without root.
func Operator(ctx context.Context) (string, error) {
	out, err := run(ctx, "debug", "prefs")
	if err != nil {
		return "", err
	}
	var p struct{ OperatorUser string }
	if err := json.Unmarshal(out, &p); err != nil {
		return "", fmt.Errorf("tailscale debug prefs: %w", err)
	}
	return p.OperatorUser, nil
}

// PublicDNS asks 1.1.1.1 directly whether host resolves. The local resolver
// would answer from MagicDNS and say yes even when strangers get NXDOMAIN.
func PublicDNS(ctx context.Context, host string) ([]string, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, "1.1.1.1:53")
		},
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return r.LookupHost(ctx, host)
}

// WaitPublic waits until url answers through every public address of host,
// the way strangers reach it. Each Funnel ingress node learns of a new
// funnel on its own schedule, so for a while after one starts some addresses
// answer and others do not; one answer proves little. An address this
// machine cannot route to at all, such as IPv6 without IPv6, is left out.
// It returns how many addresses answered out of how many were tried.
func WaitPublic(ctx context.Context, host, url string, wait time.Duration) (answered, tried int, err error) {
	addrs, err := PublicDNS(ctx, host)
	if err != nil || len(addrs) == 0 {
		return 0, 0, fmt.Errorf("%s has no public DNS record", host)
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	pending := map[string]bool{}
	for _, a := range addrs {
		pending[a] = true
	}
	tried = len(addrs)
	var last error
	for {
		var mu sync.Mutex
		var wg sync.WaitGroup
		round := make([]string, 0, len(pending))
		for a := range pending {
			round = append(round, a) // the goroutines delete from pending
		}
		for _, a := range round {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := probe(ctx, a, url)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					delete(pending, a)
				case unroutable(err):
					delete(pending, a)
					tried--
				default:
					last = fmt.Errorf("%s: %w", a, err)
				}
			}()
		}
		wg.Wait()
		if len(pending) == 0 {
			if tried == 0 {
				return 0, 0, errors.New("no public address is reachable from this machine")
			}
			return tried, tried, nil
		}
		select {
		case <-ctx.Done():
			return tried - len(pending), tried, last
		case <-time.After(time.Second):
		}
	}
}

// probe sends a HEAD for url to one address. Any reply from the share
// counts, including a password prompt or a redirect.
func probe(ctx context.Context, addr, url string) error {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(addr, "443"))
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("answered %s", resp.Status)
	}
	return nil
}

func unroutable(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.EADDRNOTAVAIL)
}

func run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "tailscale", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errors.New("tailscale is not installed")
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("tailscale %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}
