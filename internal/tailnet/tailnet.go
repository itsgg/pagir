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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/itsgg/pagir/internal/platform"
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
	return exec.Command(platform.Tailscale(), verb, fmt.Sprintf("--https=%d", httpsPort), target)
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

// ErrNoRecord means a nameserver of the host's zone does not have its
// record yet.
var ErrNoRecord = errors.New("no public DNS record")

// PublicDNS asks every nameserver of host's zone for its addresses, which
// is where strangers' resolvers learn them. The local resolver would answer
// from MagicDNS and say yes even when strangers get NXDOMAIN, and a public
// resolver would remember a miss: ts.net's are kept five minutes, for
// pagir's next lookup and for every visitor who shares that cache. The
// record counts only when every nameserver that answers has it; while any
// lacks it, the error wraps ErrNoRecord. They do not agree at once: on
// 2026-10-21 one still lacked a new record a minute after the others had it.
func PublicDNS(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	servers, err := nameservers(ctx, host)
	if err != nil {
		return nil, err
	}
	for i, s := range servers {
		servers[i] = net.JoinHostPort(s, "53")
	}
	return askAll(ctx, host, servers)
}

// askAll asks each server, an address with its port, for host at once.
func askAll(ctx context.Context, host string, servers []string) ([]string, error) {
	answers := make([]answer, len(servers))
	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Go(func() { answers[i] = lookupAt(ctx, s, host) })
	}
	wg.Wait()
	return published(host, answers)
}

// lookupAt asks one nameserver for host's addresses. Its IPv4 addresses
// must be there and its IPv6 ones are added if it gives them: on 2026-10-21
// a nameserver had a new record's IPv6 addresses before its IPv4 ones.
func lookupAt(ctx context.Context, server, host string) answer {
	r := resolver(server)
	v4, err := r.LookupIP(ctx, "ip4", host+".")
	if err != nil {
		return answer{err: err}
	}
	v6, _ := r.LookupIP(ctx, "ip6", host+".") // none, or a failed query: its IPv4 answer stands
	var a answer
	for _, ip := range append(v4, v6...) {
		a.addrs = append(a.addrs, ip.String())
	}
	return a
}

// answer is one nameserver's reply to a lookup.
type answer struct {
	addrs []string
	err   error
}

// published combines the nameservers' answers: every address any of them
// gave, or ErrNoRecord if any says the name has no IPv4 address. One that
// gave no such answer is left out: one this machine cannot reach, or one
// that answers SERVFAIL or a referral, after which a visitor's resolver
// asks another.
func published(host string, answers []answer) ([]string, error) {
	var addrs []string
	var last error
	answered := 0
	for _, a := range answers {
		var dns *net.DNSError
		switch {
		case a.err == nil:
			answered++
			addrs = append(addrs, a.addrs...)
		case errors.As(a.err, &dns) && dns.IsNotFound:
			return nil, fmt.Errorf("%s has %w", host, ErrNoRecord)
		case errors.As(a.err, &dns):
			last = errors.New(dns.Err) // its Server is the system's resolver, not the one asked
		default:
			last = a.err
		}
	}
	if answered == 0 {
		return nil, fmt.Errorf("no nameserver of %s answered: %v", host, last)
	}
	slices.Sort(addrs)
	return slices.Compact(addrs), nil
}

// nameservers finds the nameservers of the zone that holds host, a name
// such as node.tailnet.ts.net. It asks 1.1.1.1 only about the domains above
// the tailnet's: a resolver remembers a miss for minutes, and one that
// follows RFC 8020 takes a missing domain to mean nothing below it exists.
func nameservers(ctx context.Context, host string) ([]string, error) {
	for _, zone := range zonesAbove(host) {
		ns, err := resolver("1.1.1.1:53").LookupNS(ctx, zone)
		var dns *net.DNSError
		if errors.As(err, &dns) && dns.IsNotFound {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("finding the nameservers of %s: %v", zone, err)
		}
		out := make([]string, len(ns))
		for i, n := range ns {
			out[i] = n.Host
		}
		return out, nil
	}
	return nil, fmt.Errorf("no zone above %s has nameservers", host)
}

// zonesAbove lists the domains above host's tailnet domain, nearest first.
func zonesAbove(host string) []string {
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	var out []string
	for i := 2; i < len(labels)-1; i++ {
		out = append(out, strings.Join(labels[i:], ".")+".")
	}
	return out
}

// resolver sends every query to server, whatever the system's resolver
// configuration names.
func resolver(server string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, server)
		},
	}
}

// lookup is PublicDNS; tests replace it.
var lookup = PublicDNS

// dnsRetry is how long WaitDNS waits between lookups.
var dnsRetry = 2 * time.Second

// WaitDNS looks host up until every nameserver of its zone has its record,
// for up to wait, and returns its public addresses. Tailscale publishes the
// record while a funnel is on; its documentation says a new one can take up
// to ten minutes to appear, and on 2026-10-21, after two weeks without a
// funnel, it took sixteen. After each lookup that fails, miss, which may be
// nil, hears why.
func WaitDNS(ctx context.Context, host string, wait time.Duration, miss func(error)) ([]string, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var last error
	for {
		addrs, err := lookup(ctx, host)
		if err == nil {
			return addrs, nil
		}
		if ctx.Err() == nil || last == nil {
			last = err // not one the deadline cut short, unless it is all there is
		}
		if miss != nil {
			miss(err)
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return nil, parent.Err()
			}
			return nil, last
		case <-time.After(dnsRetry):
		}
	}
}

// WaitPublic waits until url answers through every one of addrs, the
// host's public addresses, the way strangers reach it. Each Funnel ingress
// node learns of a new funnel on its own schedule, so for a while after one
// starts some addresses answer and others do not; one answer proves little.
// An address this machine cannot route to at all, such as IPv6 without
// IPv6, is left out. It returns how many addresses answered out of how many
// were tried, and reports the same after every round to progress, which may
// be nil.
func WaitPublic(ctx context.Context, addrs []string, url string, wait time.Duration, progress func(answered, tried int)) (answered, tried int, err error) {
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
		if progress != nil {
			progress(tried-len(pending), tried)
		}
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

// ProbeAgent is the User-Agent of pagir's own checks, which the hub leaves
// out of access logs.
const ProbeAgent = "pagir-probe"

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
	req.Header.Set("User-Agent", ProbeAgent)
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

func unroutable(err error) bool { return platform.Unroutable(err) }

func run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, platform.Tailscale(), args...)
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
