# pagir

Share a file or folder on the internet from your terminal, through
[Tailscale Funnel](https://tailscale.com/kb/1223/funnel). pagir is Tamil for
"share".

```
$ pagir ~/Videos/talk.mp4
https://laptop.tail1234.ts.net/q4rk2x7mfh3nvd5wjtc6yp8sea/talk.mp4
file ~/Videos/talk.mp4, public, ends Tue 6 Oct 14:30, id 810d75, copied
```

The link works for anyone, ends after 24 hours unless you say otherwise,
and is already on your clipboard. "public" means pagir fetched it through
every public Funnel address before saying so. The path segment is 128
random bits, so only people you give the link to can find it.

## Use

```
pagir PATH               share a file or folder for 24h
pagir -e 2h PATH         pick the lifetime: 30m, 2h, 3d, 1w, never
pagir -f PATH            stay attached and watch requests; Ctrl+C ends it
pagir -u DIR             let visitors upload into the folder
pagir -p PASS PATH       ask visitors for a password
pagir -t PATH            your tailnet only, not the internet
pagir -H DIR             include dotfiles, which are hidden by default
pagir -q PATH            print a QR code too
pagir ls                 list active shares
pagir stop ID... | all   take shares down
pagir stop hub           take everything down and free port 443
pagir log [-f] ID        who fetched what
pagir doctor             check everything a working link needs
```

A folder page lists its files, downloads the whole folder as a zip, and
with `-u` takes uploads from the page or from `curl -T file URL`. Uploads
never overwrite: a taken name gets a number. Video and audio stream with
seeking and big downloads can resume, since range requests work.

## Install

```
go install github.com/itsgg/pagir@latest
```

or `make install`, which builds into `~/.local/bin`. pagir runs on Linux,
macOS and Windows, and needs Tailscale with MagicDNS, HTTPS certificates
and the Funnel node attribute enabled for the machine. On Linux, make your
user the Tailscale operator once, so pagir needs no sudo:

```
sudo tailscale set --operator=$USER
```

`pagir doctor` checks all of that, plus whether the machine's name resolves
in public DNS, which Tailscale publishes only after the first funnel.

CI builds and tests pagir on all three systems. The live tests, which open
real funnels, have run on Linux only so far.

## How it works

The first share starts a small background process, the hub. It owns port
443 through one foreground `tailscale funnel` (8443 through `tailscale
serve` for tailnet-only shares), proxies to its own localhost listeners,
and routes each request by its token. It keeps the funnel up after the last
share, because Tailscale's ingress nodes take up to two minutes to relearn
a funnel that was off; `pagir stop hub` ends it. If it is killed, its
tailscale processes go with it, so nothing stays published, and a crash
restarts it. Files are read as your user and confined to the shared
folder, symlinks included.

A new share is called public only once every public Funnel address
answers. When that takes longer than a few seconds, the first share after
a reboot, pagir says how many answered so far and sends a desktop
notification when the rest do. [docs/DESIGN.md](docs/DESIGN.md) has the
reasons behind each choice.

## Develop

```
make check    # gofmt, go vet, go test
make e2e      # live shares through this machine's tailscaled
```

## License

MIT
