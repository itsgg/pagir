# pagir

Shares a file or folder through Tailscale Funnel: a Go CLI, plus a hub
process that owns the funnel while anything is shared. Read
`docs/DESIGN.md` before changing how shares run; every decision there was
forced by something that failed.

## Layout

- `main.go`: commands, flags, waiting for the hub, announcing the URL.
- `supervise.go`: the detached supervisor the CLI starts. Holds hub.lock
  (which is how anything knows a hub runs), writes hub.log, restarts the hub.
- `hub.go`: the hub. Lists records once a second, routes by token, runs one
  tailscale child per lane (public on 443, kept warm; tailnet on 8443) and
  checks each new public share through every public address.
- `doctor.go`: one line per prerequisite, each naming its fix.
- `internal/web`: serves one share; `os.Root` confines it. Most tests live here.
- `internal/record`: share records (CLI writes) and `hub.json` (hub writes).
- `internal/tailnet`: the tailscale CLI, serve-config parsing, public probes.
- `internal/platform`: what differs on Linux, macOS and Windows (detaching,
  tying children to the hub, locks, boot time, clipboard, notifications).

## Commands

```
make check     # gofmt, vet, unit tests; CI runs the same on Linux, macOS, Windows
make e2e       # live: concurrent shares, public ingress, crash, expiry; run before every commit that touches hub.go, main.go or internal/tailnet
make install   # ~/.local/bin/pagir
```

## Rules

- One writer per state file: the CLI writes share records, the hub writes
  `hub.json`. The hub may delete a record, never write one.
- Nothing Linux-only outside `internal/platform`: no /proc, systemd,
  journald or Linux syscall fields elsewhere. `GOOS=darwin go vet ./...`
  and `GOOS=windows go vet ./...` must pass.
- Talk to tailscaled through the `tailscale` CLI, never the Go library: a
  library older than the daemon drops serve-config fields on write.
- Never `tailscale funnel --bg` or a path handler; see DESIGN.md.
- A claim that a share is public needs every public Funnel address to
  answer, not one.
- Before a commit, a second model reviews the diff; its findings are
  checked against the code, and the reviewer is named in the commit message.
