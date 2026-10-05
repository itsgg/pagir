// pagir puts a file or folder on the internet through Tailscale Funnel.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/itsgg/pagir/internal/qr"
	"github.com/itsgg/pagir/internal/record"
	"github.com/itsgg/pagir/internal/tailnet"
	"github.com/itsgg/pagir/internal/unit"
)

const usage = `pagir puts a file or folder on the internet through Tailscale Funnel.

Usage:
  pagir [flags] PATH       share PATH; in the background for 24h by default
  pagir ls                 list active shares
  pagir stop ID... | all   take shares down
  pagir log [-f] ID        show one share's access log
  pagir doctor             check everything a working link needs

Flags:
  -e, --expire DUR   lifetime: 30m, 2h, 3d, 1w, or never (default 24h; none with -f)
  -f, --foreground   stay attached and show requests; Ctrl+C ends the share
  -u, --upload       let visitors upload into the folder
  -p, --password PW  ask visitors for this password (any user name)
  -t, --tailnet      share on your tailnet only, not the internet
  -H, --hidden       include dotfiles, which are hidden by default
  -q, --qr           print a QR code of the URL
`

const (
	defaultLifetime = 24 * time.Hour
	publicWait      = 45 * time.Second
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("pagir: ")
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	case "ls", "list":
		err = cmdList()
	case "stop", "rm":
		err = cmdStop(args[1:])
	case "log", "logs":
		err = cmdLog(args[1:])
	case "doctor":
		if !cmdDoctor() {
			os.Exit(1)
		}
		return
	case "hub":
		cmdHub()
		return
	default:
		err = cmdShare(args)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func cmdShare(args []string) error {
	fs := flag.NewFlagSet("pagir", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	var expire, password string
	var fg, upload, tailnetOnly, hidden, showQR bool
	for _, n := range []string{"e", "expire"} {
		fs.StringVar(&expire, n, "", "")
	}
	for _, n := range []string{"p", "password"} {
		fs.StringVar(&password, n, "", "")
	}
	for n, v := range map[string]*bool{"f": &fg, "foreground": &fg, "u": &upload, "upload": &upload,
		"t": &tailnetOnly, "tailnet": &tailnetOnly, "H": &hidden, "hidden": &hidden, "q": &showQR, "qr": &showQR} {
		fs.BoolVar(v, n, false, "")
	}
	paths, err := parseInterleaved(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(paths) != 1 {
		return errors.New("share one PATH at a time (pagir -h for help)")
	}

	abs, err := filepath.Abs(paths[0])
	if err != nil {
		return err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("%s: no such file or folder", paths[0])
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is neither a file nor a folder", paths[0])
	}
	if upload && !fi.IsDir() {
		return errors.New("-u needs a folder: uploads land in it")
	}

	lifetime, never := defaultLifetime, fg
	if expire != "" {
		if lifetime, never, err = parseLifetime(expire); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	host, err := preflight(ctx, !tailnetOnly)
	if err != nil {
		return err
	}

	rec := &record.Share{
		Token:    record.NewToken(),
		Path:     abs,
		Dir:      fi.IsDir(),
		Public:   !tailnetOnly,
		Upload:   upload,
		Hidden:   hidden,
		Password: password,
		Created:  time.Now(),
		Boot:     record.CurrentBoot(),
	}
	rec.URL = "https://" + host
	if tailnetOnly {
		rec.URL += ":" + strconv.Itoa(tailnetHTTPS)
	}
	rec.URL += rec.Mount() + "/"
	if !rec.Dir {
		rec.URL += url.PathEscape(filepath.Base(abs))
	}
	if !never {
		rec.Expires = rec.Created.Add(lifetime).Round(time.Second)
	}
	if fg {
		rec.Foreground, rec.Owner = true, os.Getpid()
	}

	if err := record.Create(rec); err != nil {
		return err
	}
	id := rec.ID
	if err := ensureHub(ctx); err != nil {
		record.Remove(id)
		return err
	}
	if err := waitReady(ctx, id); err != nil {
		record.Remove(id)
		return err
	}
	announce(ctx, host, rec, showQR)
	if !fg {
		return nil
	}
	follow(ctx, rec)
	record.Remove(id)
	waitGone(id)
	return nil
}

// preflight returns this node's host name, or the reason a share cannot work.
func preflight(ctx context.Context, public bool) (string, error) {
	st, err := tailnet.GetStatus(ctx)
	if err != nil {
		return "", err
	}
	if st.BackendState != "Running" {
		return "", fmt.Errorf("tailscale is %s, not running (tailscale up)", st.BackendState)
	}
	if !st.Can("https") {
		return "", errors.New("HTTPS certificates are off for this tailnet: enable them under DNS in the admin console")
	}
	if public && !st.Can("funnel") {
		return "", errors.New("this node may not use Funnel: grant the funnel attribute in the tailnet policy (pagir doctor)")
	}
	if !st.Self.Online {
		log.Print("warning: tailscaled has lost its control connection, so a new link may not work (pagir doctor)")
	}
	sc, err := tailnet.GetServeConfig(ctx)
	if err != nil {
		return "", err
	}
	if os.Geteuid() != 0 {
		if ph := sc.PathHandlers(); len(ph) > 0 {
			return "", fmt.Errorf("a share run as root is serving %s, and tailscaled refuses every change from a non-root user while it runs; stop it first (Ctrl+C where it runs, or sudo tailscale funnel reset)", strings.Join(ph, ", "))
		}
	}
	port := tailnetHTTPS
	if public {
		port = publicHTTPS
	}
	if f := sc.Foreign(fmt.Sprintf("%s:%d", st.Host(), port), hubTarget(ctx, public)); len(f) > 0 {
		return "", fmt.Errorf("port %d is taken by %s; tailscaled gives a whole port to one listener, so stop that first", port, strings.Join(f, ", "))
	}
	return st.Host(), nil
}

// hubTarget is the local address the running hub proxies a lane to, or ""
// when no hub runs, so every listener counts as someone else's.
func hubTarget(ctx context.Context, public bool) string {
	h, err := record.LoadHub()
	if err != nil || !hubRunning(ctx, h) {
		return ""
	}
	port := h.TailnetPort
	if public {
		port = h.PublicPort
	}
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func hubActive(ctx context.Context) bool {
	up, _ := hubUp(ctx)
	return up
}

// hubRunning answers from systemd when it can be asked, and otherwise from
// whether the pid hub.json names is a live pagir, so a shell without the
// user bus does not mistake the hub's own mount for someone else's.
func hubRunning(ctx context.Context, h *record.Hub) bool {
	if up, known := hubUp(ctx); known {
		return up
	}
	return h != nil && processIsPagir(h.PID)
}

// hubUp reports whether the hub runs, and whether systemd could be asked at
// all; a shell without the user bus must not read as "no hub".
func hubUp(ctx context.Context) (up, known bool) {
	active, _, err := unit.State(ctx, hubUnit)
	if err != nil {
		return false, false
	}
	return active == "active" || active == "activating" || active == "reloading", true
}

// ensureHub starts the hub unless it runs. Two pagirs racing to start it is
// fine: the loser finds it running.
func ensureHub(ctx context.Context) error {
	if hubActive(ctx) {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := record.Dir()
	if err != nil {
		return err
	}
	// The hub gets systemd's environment, not this shell's, so it is told
	// where the records are.
	env := []string{"XDG_STATE_HOME=" + filepath.Dir(dir)}
	if err := unit.Start(ctx, hubUnit, "pagir: shares files through Tailscale", env, []string{exe, "hub"}); err != nil && !hubActive(ctx) {
		return err
	}
	return nil
}

// waitReady waits until the hub reports the share live, restarting a hub
// that exited for want of shares just before this one was written.
func waitReady(ctx context.Context, id string) error {
	deadline := time.Now().Add(mountWait + 15*time.Second)
	restarts := 0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return errors.New("interrupted")
		case <-time.After(200 * time.Millisecond):
		}
		if h, err := record.LoadHub(); err == nil {
			if st, ok := h.Shares[id]; ok {
				if st.Ready {
					return nil
				}
				if st.Error != "" {
					return errors.New(st.Error)
				}
			}
		}
		if !hubActive(ctx) {
			if restarts == 3 {
				return fmt.Errorf("the hub keeps stopping:\n%s", unit.Journal(ctx, hubUnit, 15))
			}
			restarts++
			if err := ensureHub(ctx); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("the share did not come up:\n%s", unit.Journal(ctx, hubUnit, 15))
}

// waitGone waits briefly until the hub has dropped the ids, so a stopped
// share is really gone when the command returns.
func waitGone(ids ...string) {
	ctx := context.Background()
	for range 30 {
		h, err := record.LoadHub()
		if err != nil || !hubActive(ctx) {
			return
		}
		left := false
		for _, id := range ids {
			if _, ok := h.Shares[id]; ok {
				left = true
			}
		}
		if !left {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// announce prints the URL, copies it, and for a public share waits until the
// link answers through Funnel's public ingress, which can lag the local mount
// by several seconds when no other funnel was on.
func announce(ctx context.Context, host string, rec *record.Share, showQR bool) {
	fmt.Println(rec.URL)
	if showQR {
		if code, err := qr.Render(rec.URL); err == nil {
			fmt.Print(code)
		}
	}
	copied := copyToClipboard(rec.URL)
	kind := "file"
	if rec.Dir {
		kind = "folder"
	}
	facts := []string{kind + " " + tilde(rec.Path)}
	var warning string
	if rec.Public {
		slow := time.AfterFunc(2*time.Second, func() {
			fmt.Fprintln(os.Stderr, "checking the link from every public address; the first share after a quiet spell takes a while...")
		})
		answered, tried, err := tailnet.WaitPublic(ctx, host, rec.URL, publicWait)
		slow.Stop()
		switch {
		case err == nil:
			facts = append(facts, "public")
		case answered > 0:
			facts = append(facts, fmt.Sprintf("public through %d of %d Funnel addresses", answered, tried))
			warning = fmt.Sprintf("warning: some visitors may not reach it yet (%v); the rest usually follow within a minute", err)
		default:
			facts = append(facts, "public, NOT reachable yet")
			warning = fmt.Sprintf("warning: the link works on your tailnet but not yet from the internet (%v); pagir doctor says why", err)
		}
	} else {
		facts = append(facts, "tailnet only")
	}
	if rec.Upload {
		facts = append(facts, "uploads on")
	}
	if rec.Password != "" {
		facts = append(facts, "password")
	}
	switch {
	case !rec.Expires.IsZero():
		facts = append(facts, "ends "+rec.Expires.Format("Mon 2 Jan 15:04"))
	case !rec.Foreground:
		facts = append(facts, "no expiry")
	}
	facts = append(facts, "id "+rec.ID)
	if copied {
		facts = append(facts, "copied")
	}
	fmt.Fprintln(os.Stderr, strings.Join(facts, ", "))
	if warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
}

// follow prints a foreground share's requests until Ctrl+C, or until the
// share ends some other way (expiry, pagir stop). It reads from the share's
// creation, so requests made while announce probed the public side show too.
func follow(ctx context.Context, rec *record.Share) {
	fmt.Fprintln(os.Stderr, "Ctrl+C to stop. Requests:")
	lines := make(chan string)
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(jctx, "journalctl", journalArgs(rec.Created, true)...)
	if out, err := cmd.StdoutPipe(); err == nil && cmd.Start() == nil {
		go shareLines(out, rec.ID, func(stamp time.Time, msg string) {
			select {
			case lines <- stamp.Format("15:04:05") + " " + msg:
			case <-jctx.Done():
			}
		})
		// cancel first, or Wait blocks on a journalctl that follows for ever
		defer func() { cancel(); cmd.Wait() }()
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case line := <-lines:
			fmt.Fprintln(os.Stderr, line)
		case <-tick.C:
			if _, err := record.Load(rec.ID); err != nil {
				fmt.Fprintln(os.Stderr, "the share ended")
				return
			}
		}
	}
}

func journalArgs(since time.Time, follow bool) []string {
	argv := []string{"--user", "--unit=" + hubUnit, "--no-pager", "--output=short-iso-precise"}
	if !since.IsZero() {
		argv = append(argv, "--since="+since.Add(-time.Second).Format("2006-01-02 15:04:05"))
	}
	if follow {
		argv = append(argv, "--follow")
	}
	return argv
}

// shareLines calls fn for each journal line the hub wrote about share id,
// which it prefixes with the id.
func shareLines(r io.Reader, id string, fn func(stamp time.Time, msg string)) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		head, msg, ok := strings.Cut(sc.Text(), "]: ")
		if !ok {
			continue
		}
		rest, ok := strings.CutPrefix(msg, id+" ")
		if !ok {
			continue
		}
		field, _, _ := strings.Cut(head, " ")
		stamp, err := time.Parse("2006-01-02T15:04:05.999999-07:00", field)
		if err != nil {
			stamp = time.Now()
		}
		fn(stamp.Local(), rest)
	}
}

func cmdList() error {
	ctx := context.Background()
	recs, err := record.List()
	if err != nil {
		return err
	}
	h, _ := record.LoadHub()
	active, known := hubUp(ctx)
	if !known {
		log.Print("warning: cannot ask systemd about the hub, so states are unknown")
	}
	boot := record.CurrentBoot()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	n := 0
	for _, rec := range recs {
		if rec.Boot != boot || (known && !active && time.Since(rec.Created) > 15*time.Second) {
			record.Remove(rec.ID) // nothing serves it any more
			continue
		}
		state := "starting"
		if !known {
			state = "unknown"
		} else if st, ok := h.Shares[rec.ID]; ok && active {
			switch {
			case st.Ready:
				state = "live"
			case st.Error != "":
				state = "failing"
			}
		}
		left := "never"
		switch {
		case !rec.Expires.IsZero():
			left = fmtLeft(time.Until(rec.Expires))
		case rec.Foreground:
			left = "attached"
		}
		if n == 0 {
			fmt.Fprintln(tw, "ID\tSTATE\tLEFT\tURL\tPATH")
		}
		n++
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", rec.ID, state, left, rec.URL, tilde(rec.Path))
	}
	tw.Flush()
	if n == 0 {
		fmt.Fprintln(os.Stderr, "no active shares")
	}
	return nil
}

func cmdStop(args []string) error {
	if len(args) == 0 {
		return errors.New("stop which share? pagir stop ID... or pagir stop all")
	}
	var recs []*record.Share
	if len(args) == 1 && args[0] == "all" {
		all, err := record.List()
		if err != nil {
			return err
		}
		recs = all
	} else {
		for _, id := range args {
			rec, err := record.Load(id)
			if err != nil {
				return err
			}
			recs = append(recs, rec)
		}
	}
	var ids []string
	for _, rec := range recs {
		if err := record.Remove(rec.ID); err != nil {
			return err
		}
		ids = append(ids, rec.ID)
	}
	waitGone(ids...)
	for _, rec := range recs {
		fmt.Fprintf(os.Stderr, "stopped %s (%s)\n", rec.ID, tilde(rec.Path))
	}
	return nil
}

// cmdLog shows one share's lines from the hub's journal.
func cmdLog(args []string) error {
	followFlag := false
	var ids []string
	for _, a := range args {
		if a == "-f" || a == "--follow" {
			followFlag = true
		} else {
			ids = append(ids, a)
		}
	}
	if len(ids) != 1 {
		return errors.New("log takes one share id")
	}
	id := ids[0]
	if !record.ValidID(id) {
		return fmt.Errorf("no share %q", id)
	}
	var since time.Time
	if rec, err := record.Load(id); err == nil {
		since = rec.Created
	}
	cmd := exec.Command("journalctl", journalArgs(since, followFlag)...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	shareLines(out, id, func(stamp time.Time, msg string) {
		fmt.Println(stamp.Format("2006-01-02 15:04:05"), msg)
	})
	return cmd.Wait()
}

// parseInterleaved lets flags come before or after the path, as people type
// them; everything after "--" is a path.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return append(pos, tail...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// parseLifetime reads 30m, 2h, 1h30m, 3d, 1w or never.
func parseLifetime(s string) (d time.Duration, never bool, err error) {
	if s == "never" {
		return 0, true, nil
	}
	mult := time.Duration(0)
	switch {
	case strings.HasSuffix(s, "d"):
		mult = 24 * time.Hour
	case strings.HasSuffix(s, "w"):
		mult = 7 * 24 * time.Hour
	}
	if mult != 0 {
		n, perr := strconv.ParseFloat(s[:len(s)-1], 64)
		if perr != nil {
			return 0, false, fmt.Errorf("bad lifetime %q: try 30m, 2h, 3d, 1w or never", s)
		}
		d = time.Duration(n * float64(mult))
	} else if d, err = time.ParseDuration(s); err != nil {
		return 0, false, fmt.Errorf("bad lifetime %q: try 30m, 2h, 3d, 1w or never", s)
	}
	if d <= 0 {
		return 0, false, fmt.Errorf("lifetime %q is not in the future", s)
	}
	return d, false, nil
}

func fmtLeft(d time.Duration) string {
	if d <= 0 {
		return "ending"
	}
	d = d.Round(time.Minute)
	days, hours, mins := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", max(mins, 1))
	}
}

func tilde(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rest, ok := strings.CutPrefix(p, home); ok && (rest == "" || rest[0] == '/') {
			return "~" + rest
		}
	}
	return p
}

// processIsPagir guards against a recycled pid.
func processIsPagir(pid int) bool {
	if pid <= 0 {
		return false
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return err == nil && strings.TrimSpace(string(comm)) == "pagir"
}

// clipboardTool is the copy command for this session, "" without one.
func clipboardTool() string {
	switch {
	case os.Getenv("WAYLAND_DISPLAY") != "":
		return "wl-copy"
	case os.Getenv("DISPLAY") != "":
		return "xclip"
	}
	return ""
}

func lookPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// copyToClipboard reports whether the session's clipboard took text.
func copyToClipboard(text string) bool {
	var cmd *exec.Cmd
	switch clipboardTool() {
	case "wl-copy":
		cmd = exec.Command("wl-copy")
	case "xclip":
		cmd = exec.Command("xclip", "-selection", "clipboard")
	default:
		return false
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run() == nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
