package tailnet

import "testing"

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
