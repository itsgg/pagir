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

State lives in `$XDG_STATE_HOME/pagir` (`~/.local/state/pagir` by default),
each file with one writer:

- `<id>.json`, one per share, written only by the CLI. Creating the file
  starts a share; deleting it stops one. A foreground share's terminal
  touches it every two seconds.
- `hub.json`, written only by the hub: its pid, its local ports, and each
  share's status (ready, how many public addresses answer, or why not).
- `hub.lock`, held by the supervisor while a hub runs; `hub.log`, the hub's
  log; `hub.stop`, left by `pagir stop hub` to end it; `<id>.notify`, left
  by the CLI to ask for a notification.
- tailscaled's serve config, changed only by the hub's tailscale children.

`pagir PATH` writes the record and, if no supervisor holds `hub.lock`,
starts one detached: `pagir hub`. The supervisor runs the hub itself
(`pagir hub --child`) and restarts it after a crash. Once a second, the hub
lists the records and makes its routing table match them: it drops a
record that expired, that was made before the last boot, or whose
foreground terminal stopped touching it ten seconds ago; it opens each new
share as a `web.Server`; it closes shares whose record has gone. It holds
two local listeners, one per lane:

| lane    | tailscale child                         | serves                  | runs                  |
|---------|-----------------------------------------|-------------------------|-----------------------|
| public  | `tailscale funnel --https=443 <local>`  | shares without `-t`     | while the hub runs    |
| tailnet | `tailscale serve --https=8443 <local>`  | shares with `-t`        | while it has a share  |

Each request is routed by the token in its first path segment, and only
on its own lane, so a tailnet-only token gets a 404 through Funnel.

Once a public share's lane is mounted, the hub checks it through every
public Funnel address, for up to five minutes. The CLI waits up to eight
seconds for that check. If it ends in time the CLI says "public"; if not,
it says how many addresses answered so far, leaves `<id>.notify`, and
returns, and the hub sends a desktop notification when the check ends.

## Decisions

**A hub, because tailscaled gives a whole port to one foreground
listener.** The first design ran one process per share, each with its own
foreground `tailscale funnel --set-path /<token>`. The second share failed
with `listener already exists for port 443`. Funnel offers three ports, so
per-share listeners cap out at three shares. The hub owns the port once and
routes by token instead.

**The funnel stays up after the last share.** The first hub exited ten
seconds after the last share, so the next share started the funnel cold.
Funnel's ingress nodes then take their time: one share waited 45 seconds
and still answered through only three of four addresses, the last
answering between 46 and 110 seconds after the funnel came up. A share on
a funnel already up was public through all four addresses in 1.6 seconds.
The cost is that no other funnel can use port 443 while pagir's hub runs;
`pagir stop hub` frees it.

**pagir supervises itself instead of using systemd.** The hub first ran as
a transient systemd user unit, which gave restarts and a journal for free
and tied pagir to Linux. A detached supervisor gives the same on every
system: an exclusive lock on `hub.lock` both keeps a second hub out and
tells the CLI whether one runs, and the lock goes when the process does,
however it ends. Logs go to `hub.log`, which `pagir log` reads.

**Foreground tailscale children, never `--bg`.** tailscaled ties a
foreground mount to the CLI's connection and removes it when that
connection closes, so a mount lives exactly as long as the child holding
it. The children die with the hub: Pdeathsig on Linux, a kill-on-close job
object on Windows, and on macOS, which has neither, the supervisor kills
the dead hub's process group. A `--bg` mount outlives its owner, survives
reboots in tailscaled's prefs, and would need cleanup code that can itself
fail.

**One writer per file.** The hub never writes a share record, only deletes
one. If it wrote readiness into the record, a `pagir stop` that deleted
the file just before the hub's write would see the share come back. Status
lives in `hub.json`, which only the hub writes.

**Files, not signals.** The CLI could poke the hub with a signal, but a
hub that has just started, before it installs its handler, would die of
it, and Windows has no SIGHUP or SIGTERM to send. Listing a few small
files once a second costs nothing. `stop` waits until `hub.json` drops the
share, so it returns when the share is really gone; `stop hub` leaves
`hub.stop` and waits for the lock to go.

**Records die with the boot.** A record made before the machine last
booted is dropped, by comparing its creation time with the boot time
(`/proc/stat` on Linux, `kern.boottime` on macOS, the tick count on
Windows). Shares are running things: after a reboot no hub runs, and the
next share would otherwise start a hub that silently republishes every old
record.

**A foreground share is a file its terminal touches.** Checking the
terminal's pid would need /proc to rule out a reused pid. A modification
time older than ten seconds means the terminal is gone, on any system.

**Public means every Funnel address answers.** After a funnel starts, each
ingress node learns of it on its own schedule; for tens of seconds one of
the host's public addresses answered while another dropped the TLS
handshake. The hub probes each address from public DNS (asked of 1.1.1.1
directly, since the local resolver answers from MagicDNS), and a share is
"public" only when all answer. An address this machine cannot route to,
such as IPv6 without IPv6, is left out. Its probes carry the User-Agent
`pagir-probe` and stay out of the access log.

**Say what was checked, and no more.** The first version waited 45
seconds, then printed the raw Go error and "the rest usually follow within
a minute", which was a guess. Now the CLI waits eight seconds, states how
many addresses answered, and the notification reports the outcome when it
is known.

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
