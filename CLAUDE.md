# pagir

Shares a file or folder through Tailscale Funnel: a Go CLI, plus a hub
process that owns the funnel while anything is shared. Read
`docs/DESIGN.md` before changing how shares run; every decision there was
forced by something that failed.

## Layout

- `main.go`: commands, flags, waiting for the hub, announcing the URL.
- `hub.go`: the hub. Lists records once a second, routes by token, runs one
  tailscale child per lane (public on 443, tailnet on 8443).
- `doctor.go`: one line per prerequisite, each naming its fix.
- `internal/web`: serves one share; `os.Root` confines it. Most tests live here.
- `internal/record`: share records (CLI writes) and `hub.json` (hub writes).
- `internal/tailnet`: the tailscale CLI, serve-config parsing, public probes.
- `internal/unit`: systemd-run and systemctl for the hub unit.

## Commands

```
make check     # gofmt, vet, unit tests; CI runs this
make e2e       # live: concurrent shares, public ingress, crash, expiry; run before every commit that touches hub.go, main.go or internal/tailnet
make install   # ~/.local/bin/pagir
```

## Rules

- One writer per state file: the CLI writes share records, the hub writes
  `hub.json`. The hub may delete a record, never write one.
- Talk to tailscaled through the `tailscale` CLI, never the Go library: a
  library older than the daemon drops serve-config fields on write.
- Never `tailscale funnel --bg` or a path handler; see DESIGN.md.
- A claim that a share is public needs every public Funnel address to
  answer, not one.
- Before a commit, a second model reviews the diff; its findings are
  checked against the code, and the reviewer is named in the commit message.
