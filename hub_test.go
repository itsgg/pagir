package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/itsgg/pagir/internal/record"
	"github.com/itsgg/pagir/internal/tailnet"
)

// testHub is a hub with its lanes' listeners and no tailscale children.
func testHub(t *testing.T, logs *bytes.Buffer) *hub {
	h := &hub{
		host:   "host.tail0000.ts.net",
		logger: log.New(logs, "", 0),
		byTok:  map[string]*live{},
		byID:   map[string]*live{},
		failed: map[string]string{},
	}
	for i := range h.lanes {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		h.lanes[i] = &lane{public: i == 0, ln: ln}
	}
	return h
}

func stubWaits(t *testing.T) {
	d, p := waitForDNS, waitForAddrs
	t.Cleanup(func() { waitForDNS, waitForAddrs = d, p })
}

func TestRunCheckWaitsForDNS(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stubWaits(t)
	var logs bytes.Buffer
	h := testHub(t, &logs)
	rec := &record.Share{ID: "aaaaaa", Public: true, URL: "https://host.tail0000.ts.net/tok/a.txt"}
	c := &publicCheck{started: time.Now()}
	h.byID[rec.ID] = &live{rec: rec, check: c}
	want := map[string]*record.Share{rec.ID: rec}

	waitForDNS = func(_ context.Context, host string, _ time.Duration, miss func(error)) ([]string, error) {
		miss(fmt.Errorf("%s has %w", host, tailnet.ErrNoRecord))
		// While the record is missing, hub.json says so, for the CLI.
		h.publish(want)
		if hs, err := record.LoadHub(); err != nil || !hs.Shares[rec.ID].NoDNS {
			t.Errorf("hub.json while the record is missing: %+v %v", hs, err)
		}
		return []string{"192.0.2.1"}, nil
	}
	waitForAddrs = func(_ context.Context, addrs []string, _ string, _ time.Duration, progress func(int, int)) (int, int, error) {
		h.mu.Lock()
		noDNS := c.noDNS
		h.mu.Unlock()
		if noDNS || len(addrs) != 1 || addrs[0] != "192.0.2.1" {
			t.Errorf("address check started with noDNS %v, addresses %v", noDNS, addrs)
		}
		progress(1, 1)
		return 1, 1, nil
	}
	h.runCheck(context.Background(), rec, c)
	if !c.done || c.err != "" || c.of != 1 || c.noDNS {
		t.Errorf("check after the record appeared: %+v", c)
	}
	if !strings.Contains(logs.String(), "host.tail0000.ts.net is in public DNS after") {
		t.Errorf("log: %q", logs.String())
	}
	h.publish(want)
	if hs, _ := record.LoadHub(); !hs.Shares[rec.ID].Public || hs.Shares[rec.ID].NoDNS {
		t.Errorf("hub.json once public: %+v", hs.Shares[rec.ID])
	}
}

func TestRunCheckEndsWithoutDNS(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stubWaits(t)
	var logs bytes.Buffer
	h := testHub(t, &logs)
	rec := &record.Share{ID: "bbbbbb", Public: true, URL: "https://host.tail0000.ts.net/tok/a.txt"}
	c := &publicCheck{started: time.Now()}
	h.byID[rec.ID] = &live{rec: rec, check: c}

	waitForDNS = func(_ context.Context, host string, _ time.Duration, miss func(error)) ([]string, error) {
		err := fmt.Errorf("%s has %w", host, tailnet.ErrNoRecord)
		miss(err)
		return nil, err
	}
	waitForAddrs = func(context.Context, []string, string, time.Duration, func(int, int)) (int, int, error) {
		t.Error("addresses checked without a DNS record")
		return 0, 0, nil
	}
	h.runCheck(context.Background(), rec, c)
	if !c.done || c.noDNS || c.of != 0 || !strings.Contains(c.err, "no public DNS record") {
		t.Errorf("check after the wait ran out: %+v", c)
	}
	h.publish(map[string]*record.Share{rec.ID: rec})
	if st := mustHub(t).Shares[rec.ID]; st.Public || st.NoDNS || st.PublicErr == "" {
		t.Errorf("hub.json after the wait ran out: %+v", st)
	}
}

func mustHub(t *testing.T) *record.Hub {
	h, err := record.LoadHub()
	if err != nil {
		t.Fatal(err)
	}
	return h
}
