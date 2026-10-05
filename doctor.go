package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/itsgg/pagir/internal/platform"
	"github.com/itsgg/pagir/internal/record"
	"github.com/itsgg/pagir/internal/tailnet"
)

// cmdDoctor checks everything a working public link needs and reports whether
// nothing failed. Each line names the fix.
func cmdDoctor() bool {
	ctx := context.Background()
	ok := true
	say := func(level, format string, a ...any) {
		if level == "fail" {
			ok = false
		}
		fmt.Printf("%-5s %s\n", level, fmt.Sprintf(format, a...))
	}

	st, err := tailnet.GetStatus(ctx)
	if err != nil {
		say("fail", "%v", err)
		return false
	}
	if st.BackendState != "Running" {
		say("fail", "tailscale is %s, not running: tailscale up", st.BackendState)
		return false
	}
	host := st.Host()
	say("ok", "tailscale is running as %s", host)

	if st.Self.Online {
		say("ok", "connected to the control server")
	} else {
		say("fail", "no working connection to the control server, so certificates and DNS cannot update; "+
			"after a Wi-Fi roam tailscaled can hold a dead one for about 15 minutes: restart tailscaled, or kill that one socket with ss -K")
	}
	for _, h := range st.Health {
		say("warn", "tailscale health: %s", strings.TrimSpace(h))
	}

	if st.Can("https") {
		say("ok", "HTTPS certificates are enabled")
	} else {
		say("fail", "HTTPS certificates are off: enable them under DNS in the admin console")
	}
	if st.Can("funnel") {
		say("ok", "this node may use Funnel")
	} else {
		say("fail", "this node may not use Funnel: add the funnel node attribute in the tailnet policy")
	}

	me := currentUser()
	switch {
	case runtime.GOOS != "linux":
		// The operator is a Linux idea: elsewhere the CLI talks to the app
		// as the signed-in user.
	case os.Geteuid() == 0:
		say("ok", "running as root")
	default:
		if op, err := tailnet.Operator(ctx); err != nil {
			say("warn", "cannot read the operator: %v", err)
		} else if op == me {
			say("ok", "%s is the tailscale operator, so no sudo is needed", me)
		} else {
			say("fail", "%s is not the tailscale operator: sudo tailscale set --operator=%s", me, me)
		}
	}

	running := hubRunning()
	if sc, err := tailnet.GetServeConfig(ctx); err != nil {
		say("warn", "cannot read the serve config: %v", err)
	} else {
		if ph := sc.PathHandlers(); len(ph) > 0 {
			say("fail", "a share run as root (%s) blocks every change from %s until it stops", strings.Join(ph, ", "), me)
		} else {
			say("ok", "no root-run share is blocking changes")
		}
		for _, l := range []struct {
			public bool
			port   int
			kind   string
		}{{true, publicHTTPS, "public"}, {false, tailnetHTTPS, "tailnet-only"}} {
			if f := sc.Foreign(fmt.Sprintf("%s:%d", host, l.port), hubTarget(l.public)); len(f) > 0 {
				say("fail", "port %d is taken by %s, so pagir cannot publish %s shares until it stops", l.port, strings.Join(f, ", "), l.kind)
			} else {
				say("ok", "port %d is free for %s shares", l.port, l.kind)
			}
		}
	}

	if running {
		h, _ := record.LoadHub()
		say("ok", "the hub is running with %d share(s) and holds port %d for Funnel; pagir stop hub frees it", len(h.Shares), publicHTTPS)
	} else {
		say("ok", "the hub is not running; it starts with the first share")
	}

	if addrs, err := tailnet.PublicDNS(ctx, host); err == nil && len(addrs) > 0 {
		say("ok", "%s resolves publicly (%s)", host, strings.Join(addrs, ", "))
	} else {
		say("warn", "%s has no public DNS record, so only your tailnet can open links; "+
			"Tailscale publishes it while a funnel is on, which can take minutes the first time", host)
	}

	if dir, err := record.Dir(); err != nil {
		say("fail", "no state directory: %v", err)
	} else {
		say("ok", "state in %s", tilde(dir))
	}
	say("ok", "tailscale CLI at %s", platform.Tailscale())

	switch clip := platform.Clipboard(); {
	case clip == "":
		say("ok", "no clipboard in this session, so URLs are printed, not copied")
	case lookPath(clip):
		say("ok", "URLs are copied with %s", clip)
	default:
		say("warn", "%s is missing, so URLs are not copied", clip)
	}
	return ok
}

func lookPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}
