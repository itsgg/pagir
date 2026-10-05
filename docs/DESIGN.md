# pagir design

pagir (Tamil for "share") puts a file or folder on the internet through
Tailscale Funnel with one command, and takes it down again. Started
2026-10-05.

## What it does

```
pagir PATH               share in the background for 24h; URL printed and copied
pagir -e 2h PATH         lifetime; -e never keeps it until stopped
pagir -f PATH            stay attached and show requests; Ctrl+C stops it
pagir -u DIR             visitors can upload into the folder
pagir -p PASS PATH       HTTP basic auth; any user name, this password
pagir -t PATH            tailnet only (tailscale serve on :8443, not funnel)
pagir -H DIR             include dotfiles, which are hidden by default
pagir -q PATH            also print a QR code of the URL
pagir ls                 active shares: id, state, time left, URL, path
pagir stop ID... | all   take shares down
pagir log [-f] ID        the access log of one share
pagir doctor             everything that has to be true for a link to work
```

A file is served at `https://<host>/<token>/<name>`, a folder at
`https://<host>/<token>/`, a tailnet-only share on port 8443. The token is
128 random bits. A folder page lists its entries, offers the whole folder
as a zip, and takes uploads when `-u` is on; `curl -T file URL` uploads too.

## How it runs

Three kinds of state, each with one writer:

- `$XDG_STATE_HOME/pagir/<id>.json`, one per share, written only by the
  CLI. Creating the file starts a share; deleting it stops one.
- `hub.json`, written only by the hub: its pid, its local ports, and each
  share's status (ready, or why not).
- tailscaled's serve config, changed only by the hub's tailscale children.

The hub is `pagir hub`, run as the transient user unit `pagir.service`. The
CLI starts it when it writes a record and finds no hub. Once a second, the
hub lists the records and makes its routing table match them: it drops a
record that expired, that was made before the last reboot, or whose
foreground owner has died; it opens each new share as a `web.Server`; it
closes shares whose record has gone. It holds two local listeners, one per
lane:

| lane    | tailscale child                         | serves                  |
|---------|-----------------------------------------|-------------------------|
| public  | `tailscale funnel --https=443 <local>`  | shares without `-t`     |
| tailnet | `tailscale serve --https=8443 <local>`  | shares with `-t`        |

A child runs only while its lane has a share. Each request is routed by
the token in its first path segment, and only on its own lane, so a
tailnet-only token gets a 404 through Funnel. When no record has been
wanted for 10 seconds the hub exits.

`pagir PATH` writes the record, makes sure the hub runs, waits for
`hub.json` to say the share is ready, and then for a public share waits
until the URL answers through every public Funnel address before it says
"public".

## Decisions

**A hub, because tailscaled gives a whole port to one foreground
listener.** The first design ran one process per share, each with its own
foreground `tailscale funnel --set-path /<token>`. The second share failed
with `listener already exists for port 443`. Funnel offers three ports, so
per-share listeners cap out at three shares. The hub owns the port once and
routes by token instead. It is a daemon only while something is shared: it
starts with the first share and exits after the last, so nothing listens
on 443 while nothing is shared.

**Foreground tailscale children, never `--bg`.** tailscaled ties a
foreground mount to the CLI's connection and removes it when that
connection closes. If the hub is killed outright, its children die with it
(Pdeathsig) and the mounts go. Kill the machine's power and the mounts are
still gone after boot. A `--bg` mount outlives its owner, survives reboots
in tailscaled's prefs, and would need cleanup code that can itself fail.

**One writer per file.** The hub never writes a share record, only deletes
one. If it wrote readiness into the record, a `pagir stop` that deleted
the file just before the hub's write would see the share come back. Status
lives in `hub.json`, which only the hub writes.

**Polling, not signals.** The CLI could poke the hub with SIGHUP, but a
hub that has just started, before it installs its handler, would die of
it. Listing a few small files once a second costs nothing, and `stop`
waits until `hub.json` drops the share, so it returns when the share is
really gone.

**Records die with the boot.** Each record carries the kernel's boot id.
Shares are running things: after a reboot no hub runs, and the next share
would otherwise start a hub that silently republishes every old record.

**Public means every Funnel address answers.** After a funnel starts, each
ingress node learns of it on its own schedule; for tens of seconds one of
the host's public addresses answered while another dropped the TLS
handshake. pagir probes each address from public DNS (asked of 1.1.1.1
directly, since the local resolver answers from MagicDNS) and reports how
many answered. An address this machine cannot route to, such as IPv6
without IPv6, is left out.

**The tailscale CLI, not the Go client library.** The library would let the
hub set the serve config itself, but the config is read, modified and
written back whole, and a library older than the running tailscaled would
drop fields it does not know. The CLI on the machine always matches its
daemon. It also keeps pagir to one small binary with one tiny dependency
(`rsc.io/qr`).

**A local port, never a path handler.** tailscaled refuses to serve a file
or directory for a non-root user unless that user can run `sudo
tailscale`, and it checks the whole serve config, so one root-run path
share blocks every operator change. Proxying to localhost has no such
check. It also means the files are read by the user, not by root.

**pagir serves the files itself.** Go's `net/http` does range requests and
conditional GETs. `os.Root` confines every open to the shared folder,
symlinks included, so `..` and a link to `/etc` both fail. A link with an
absolute target is never followed, even one pointing inside the share;
that is os.Root's rule, and a relative link works. miniserve was the first
plan, under a shell script; once the language was Go it would have been a
second process and dependency for what the standard library already does.

**24h by default.** A forgotten public share is the one mistake this tool
can make, so a share ends on its own unless asked to stay (`-e never`).
Foreground shares end with their terminal, so they have no default expiry.

**Dotfiles hidden by default.** `.git` and `.env` in a shared project
folder are the classic leak. Hidden means absent from listings and zips
and refused when asked for by name. `-H` includes them.

## Tailscale behaviour worth knowing

Observed with Tailscale 1.102 on Linux while building pagir; each one
shaped the code above.

- **One foreground listener per port.** A second foreground `tailscale
  funnel` or `serve` on a port already held fails with `listener already
  exists for port 443`, whatever its `--set-path`.
- **Ingress nodes learn of a funnel at different times.** For tens of
  seconds after a funnel starts, one public address of the host can answer
  while another drops the TLS handshake.
- **Public DNS can lag the first funnel.** A node's name resolves publicly
  only once Tailscale publishes its record. On the machine pagir was built
  on, it was still missing a minute after the first funnel's certificate
  arrived.
- **`--set-path` is stripped before proxying.** A mount at `/x` proxying
  to `http://127.0.0.1:8080/y` sends `/x/a` to `/y/a`. The hub mounts `/`,
  so it sees the token itself.
- **A dead control connection stalls certificates.** After a Wi-Fi roam
  that kept the same IP, tailscaled kept reusing a dead TCP connection to
  its control server until the kernel gave up retransmitting, about 15
  minutes. The node shows as offline in `tailscale status`, and a new
  funnel waits on "Fetching TLS certificate". `pagir doctor` reports the
  offline state; killing that one socket with `ss -K` makes tailscaled
  redial at once.

`make e2e` found the first two and a race in following a foreground
share's log; no unit test could have.

## Not in v1

One-time links, download counts, `extend`, a file-manager action, a
notification on first download, an Akshi verb that sends the link over
Telegram, sharing stdin.
