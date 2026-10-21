package tailnet

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"slices"
	"testing"
	"time"
)

// Captured from `tailscale serve status --json` on 2026-10-05 (names changed), while a root
// `tailscale funnel ~/Public/` ran in the foreground.
const rootForeground = `{
  "Foreground": {
    "1361c0bd523eb960": {
      "TCP": {"443": {"HTTPS": true}},
      "Web": {"host.tail0000.ts.net:443": {"Handlers": {"/": {"Path": "/home/me/Public"}}}},
      "AllowFunnel": {"host.tail0000.ts.net:443": true}
    }
  }
}`

const proxyForeground = `{
  "Foreground": {
    "aa": {"Web": {"host.tail0000.ts.net:443": {"Handlers": {"/tok": {"Proxy": "http://127.0.0.1:4000/tok"}}}}}
  }
}`

func TestServeConfig(t *testing.T) {
	c, err := ParseServeConfig([]byte(rootForeground))
	if err != nil {
		t.Fatal(err)
	}
	if ph := c.PathHandlers(); len(ph) != 1 || ph[0] != "https://host.tail0000.ts.net/ -> /home/me/Public" {
		t.Errorf("path handlers %v", ph)
	}
	if !c.HasMount("host.tail0000.ts.net:443", "/") || c.HasMount("host.tail0000.ts.net:443", "/tok") {
		t.Error("HasMount on the root share")
	}

	c, err = ParseServeConfig([]byte(proxyForeground))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.PathHandlers()) != 0 {
		t.Error("a proxy counted as a path handler")
	}
	if !c.HasMount("host.tail0000.ts.net:443", "/tok") || c.HasMount("other.ts.net:443", "/tok") || c.HasMount("host.tail0000.ts.net:8443", "/tok") {
		t.Error("HasMount on a proxy share")
	}
	if f := c.Foreign("host.tail0000.ts.net:443", "http://127.0.0.1:4000/tok"); len(f) != 0 {
		t.Errorf("our own proxy counted as foreign: %v", f)
	}
	if f := c.Foreign("host.tail0000.ts.net:443", "http://127.0.0.1:5000"); len(f) != 1 {
		t.Errorf("someone else's proxy not counted as foreign: %v", f)
	}

	for _, empty := range []string{"", "{}\n", "  \n"} {
		c, err := ParseServeConfig([]byte(empty))
		if err != nil || c.HasMount("x:443", "/") || len(c.PathHandlers()) != 0 {
			t.Errorf("empty config %q: %v", empty, err)
		}
	}
}

func TestStatus(t *testing.T) {
	s, err := ParseStatus([]byte(`{"BackendState":"Running","Self":{"DNSName":"host.tail0000.ts.net.","Online":true,"CapMap":{"funnel":null,"https":null}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Host() != "host.tail0000.ts.net" || !s.Can("funnel") || !s.Can("https") || s.Can("ssh") {
		t.Errorf("status %+v", s)
	}
}

func TestZonesAbove(t *testing.T) {
	for host, want := range map[string][]string{
		"host.tail0000.ts.net":  {"ts.net."},
		"host.tail0000.ts.net.": {"ts.net."},
		"a.b.c.example.co.uk":   {"c.example.co.uk.", "example.co.uk.", "co.uk."},
		"host.tail0000":         nil,
	} {
		if got := zonesAbove(host); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
}

func TestPublished(t *testing.T) {
	missing := &net.DNSError{Err: "no such host", IsNotFound: true}
	silent := &net.DNSError{Err: "i/o timeout", IsTimeout: true, Server: "127.0.0.53:53"}
	unroutable := errors.New("connect: network is unreachable")

	addrs, err := published("h", []answer{{addrs: []string{"2.2.2.2", "1.1.1.1"}}, {addrs: []string{"1.1.1.1", "::1"}}, {err: silent}})
	if err != nil || !slices.Equal(addrs, []string{"1.1.1.1", "2.2.2.2", "::1"}) {
		t.Errorf("every answer has it: %v %v", addrs, err)
	}
	// One nameserver without the record is enough to call it missing:
	// a visitor's resolver may ask that one.
	if _, err := published("h", []answer{{addrs: []string{"1.1.1.1"}}, {err: missing}}); !errors.Is(err, ErrNoRecord) {
		t.Errorf("one lacks it: %v", err)
	}
	_, err = published("h", []answer{{err: silent}, {err: unroutable}})
	if err == nil || errors.Is(err, ErrNoRecord) {
		t.Errorf("nobody answered: %v", err)
	}
	if err != nil && (err.Error() != "no nameserver of h answered: connect: network is unreachable") {
		t.Errorf("nobody answered says %q", err)
	}
	if _, err := published("h", []answer{{err: unroutable}, {err: silent}}); err == nil || err.Error() != "no nameserver of h answered: i/o timeout" {
		t.Errorf("a DNS error names the system's resolver: %v", err)
	}
	if _, err := published("h", nil); err == nil {
		t.Error("no nameservers counted as published")
	}
}

func TestWaitDNS(t *testing.T) {
	defer func(l func(context.Context, string) ([]string, error), r time.Duration) { lookup, dnsRetry = l, r }(lookup, dnsRetry)
	dnsRetry = time.Millisecond
	missing := errors.Join(ErrNoRecord)
	calls := 0
	lookup = func(context.Context, string) ([]string, error) {
		calls++
		if calls < 4 {
			return nil, missing
		}
		return []string{"1.1.1.1"}, nil
	}
	var misses []error
	addrs, err := WaitDNS(context.Background(), "h", time.Minute, func(err error) { misses = append(misses, err) })
	if err != nil || !slices.Equal(addrs, []string{"1.1.1.1"}) || len(misses) != 3 {
		t.Errorf("a record that appears after three misses: %v %v, %d misses", addrs, err, len(misses))
	}

	calls = -1000
	start := time.Now()
	_, err = WaitDNS(context.Background(), "h", 30*time.Millisecond, nil)
	if !errors.Is(err, ErrNoRecord) || time.Since(start) > 500*time.Millisecond {
		t.Errorf("a record that never appears: %v after %s", err, time.Since(start))
	}

	// A lookup that hangs is cut off at the deadline, and the wait reports
	// the last answer it had, not the deadline.
	dnsRetry = time.Millisecond
	calls = 0
	lookup = func(ctx context.Context, _ string) ([]string, error) {
		calls++
		if calls == 1 {
			return nil, missing
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start = time.Now()
	_, err = WaitDNS(context.Background(), "h", 50*time.Millisecond, nil)
	if !errors.Is(err, ErrNoRecord) || time.Since(start) > 500*time.Millisecond {
		t.Errorf("a hanging lookup: %v after %s", err, time.Since(start))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = -1000
	if _, err := WaitDNS(ctx, "h", time.Minute, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait: %v", err)
	}
}

// fakeNS is an authoritative nameserver for one name: it answers A queries
// with a and AAAA queries with aaaa, a type with no addresses gets an empty
// answer, nx answers NXDOMAIN to all, servfail SERVFAIL to all and
// failAAAA SERVFAIL to AAAA queries, and silent never answers.
type fakeNS struct {
	a, aaaa                        []string
	nx, servfail, failAAAA, silent bool
}

// start serves f on a local UDP port and returns its address.
func (f fakeNS) start(t *testing.T) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if resp := f.answer(buf[:n]); resp != nil && !f.silent {
				pc.WriteTo(resp, from)
			}
		}
	}()
	return pc.LocalAddr().String()
}

func (f fakeNS) answer(q []byte) []byte {
	// A 12-byte header, then the question: labels, a zero byte, type, class.
	end := 12
	for end < len(q) && q[end] != 0 {
		end += int(q[end]) + 1
	}
	end += 5
	if end > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[end-4:])
	var rdata [][]byte
	switch {
	case f.nx:
	case qtype == 1:
		for _, a := range f.a {
			rdata = append(rdata, net.ParseIP(a).To4())
		}
	case qtype == 28:
		for _, a := range f.aaaa {
			rdata = append(rdata, net.ParseIP(a).To16())
		}
	}
	flags := uint16(0x8400) // a response, authoritative
	switch {
	case f.nx:
		flags |= 3 // NXDOMAIN
	case f.servfail, f.failAAAA && qtype == 28:
		flags |= 2 // SERVFAIL
		rdata = nil
	}
	resp := binary.BigEndian.AppendUint16(nil, binary.BigEndian.Uint16(q))
	resp = binary.BigEndian.AppendUint16(resp, flags)
	resp = binary.BigEndian.AppendUint16(resp, 1)
	resp = binary.BigEndian.AppendUint16(resp, uint16(len(rdata)))
	resp = append(resp, 0, 0, 0, 0)
	resp = append(resp, q[12:end]...)
	for _, rd := range rdata {
		resp = append(resp, 0xc0, 12) // the question's name
		resp = binary.BigEndian.AppendUint16(resp, qtype)
		resp = binary.BigEndian.AppendUint16(resp, 1)
		resp = binary.BigEndian.AppendUint32(resp, 60)
		resp = binary.BigEndian.AppendUint16(resp, uint16(len(rd)))
		resp = append(resp, rd...)
	}
	return resp
}

func TestAskAll(t *testing.T) {
	full := fakeNS{a: []string{"192.0.2.2", "192.0.2.1"}, aaaa: []string{"2001:db8::1"}}.start(t)
	v4only := fakeNS{a: []string{"192.0.2.1"}}.start(t)
	v6only := fakeNS{aaaa: []string{"2001:db8::1"}}.start(t)
	nx := fakeNS{nx: true}.start(t)
	silent := fakeNS{silent: true}.start(t)
	servfail := fakeNS{servfail: true}.start(t)
	failAAAA := fakeNS{a: []string{"192.0.2.1"}, aaaa: []string{"2001:db8::1"}, failAAAA: true}.start(t)
	all := []string{"192.0.2.1", "192.0.2.2", "2001:db8::1"}
	for _, c := range []struct {
		name     string
		servers  []string
		want     []string
		noRecord bool
	}{
		{"every server has it", []string{full, full}, all, false},
		{"addresses are merged", []string{v4only, full}, all, false},
		{"no IPv6 is fine", []string{v4only}, []string{"192.0.2.1"}, false},
		// As one nameserver was on 2026-10-21, a minute into a new record.
		{"IPv6 without IPv4 is missing", []string{full, v6only}, nil, true},
		{"one NXDOMAIN is missing", []string{full, nx}, nil, true},
		{"a silent server is left out", []string{full, silent}, all, false},
		// A visitor's resolver asks another nameserver after a SERVFAIL.
		{"a SERVFAIL server is left out", []string{full, servfail}, all, false},
		{"a failed AAAA query keeps the IPv4 answer", []string{failAAAA}, []string{"192.0.2.1"}, false},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		addrs, err := askAll(ctx, "host.tail0000.ts.net", c.servers)
		cancel()
		if c.noRecord {
			if !errors.Is(err, ErrNoRecord) {
				t.Errorf("%s: %v %v, want ErrNoRecord", c.name, addrs, err)
			}
			continue
		}
		if err != nil || !slices.Equal(addrs, c.want) {
			t.Errorf("%s: %v %v, want %v", c.name, addrs, err, c.want)
		}
	}
}
