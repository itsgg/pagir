// pagir puts a file or folder on the internet through Tailscale Funnel.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/itsgg/pagir/internal/platform"
	"github.com/itsgg/pagir/internal/qr"
	"github.com/itsgg/pagir/internal/record"
	"github.com/itsgg/pagir/internal/tailnet"
)

const usage = `pagir puts a file or folder on the internet through Tailscale Funnel.

Usage:
  pagir [flags] PATH       share PATH; in the background for 24h by default
  pagir ls                 list active shares
  pagir stop ID... | all   take shares down
  pagir stop hub           take every share and the hub down, freeing port 443
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
	announceWait    = 8 * time.Second // how long a new share waits for the public check before handing it to the hub
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
		if len(args) > 1 && args[1] == "--child" {
			cmdHub()
		} else {
			cmdSupervise()
		}
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
		Token:      record.NewToken(),
		Path:       abs,
		Dir:        fi.IsDir(),
		Public:     !tailnetOnly,
		Upload:     upload,
		Hidden:     hidden,
		Password:   password,
		Created:    time.Now(),
		Foreground: fg,
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

	if err := record.Create(rec); err != nil {
		return err
	}
	id := rec.ID
	if fg {
		go attached(ctx, id)
	}
	if err := ensureHub(); err != nil {
		record.Remove(id)
		return err
	}
	if err := waitReady(ctx, id); err != nil {
		record.Remove(id)
		return err
	}
	public := announce(ctx, rec, showQR)
	if !fg {
		return nil
	}
	follow(ctx, rec, public)
	record.Remove(id)
	waitGone(id)
	return nil
}

// attached touches a foreground share's record while its terminal lives;
// the hub ends a share whose record has gone untouched for attachedFor.
func attached(ctx context.Context, id string) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			record.Touch(id)
		}
	}
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
	if f := sc.Foreign(fmt.Sprintf("%s:%d", st.Host(), port), hubTarget(public)); len(f) > 0 {
		return "", fmt.Errorf("port %d is taken by %s; tailscaled gives a whole port to one listener, so stop that first", port, strings.Join(f, ", "))
	}
	return st.Host(), nil
}

// hubTarget is the local address the running hub proxies a lane to, or ""
// when no hub runs, so every listener counts as someone else's.
func hubTarget(public bool) string {
	if !hubRunning() {
		return ""
	}
	h, err := record.LoadHub()
	if err != nil {
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

// ensureHub starts the hub unless one runs. Two pagirs racing to start it
// is fine: the second supervisor finds the lock taken and leaves.
func ensureHub() error {
	if hubRunning() {
		return nil
	}
	return startHub()
}

// waitReady waits until the hub reports the share live, starting the hub
// again if it is not running a moment after it should be.
func waitReady(ctx context.Context, id string) error {
	deadline := time.Now().Add(mountWait + 15*time.Second)
	starts, down := 0, 0
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
		if hubRunning() {
			down = 0
			continue
		}
		if down++; down < 5 { // a supervisor takes a moment to lock
			continue
		}
		if starts == 3 {
			return fmt.Errorf("the hub keeps stopping; the end of its log:\n%s", logTail(15))
		}
		starts, down = starts+1, 0
		if err := startHub(); err != nil {
			return err
		}
	}
	return fmt.Errorf("the share did not come up; the end of the hub's log:\n%s", logTail(15))
}

// waitGone waits briefly until the hub has dropped the ids, so a stopped
// share is really gone when the command returns.
func waitGone(ids ...string) {
	for range 30 {
		h, err := record.LoadHub()
		if err != nil || !hubRunning() {
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

// announce prints the URL and one line of facts, every one of them checked.
// For a public share it waits a few seconds for the hub's check of every
// public address; if that is not done, it says how far it got and leaves a
// mark so the hub sends a notification when the check ends.
// It returns whether the public check is over, so a foreground share does
// not report it twice.
func announce(ctx context.Context, rec *record.Share, showQR bool) (checked bool) {
	fmt.Println(rec.URL)
	if showQR {
		if code, err := qr.Render(rec.URL); err == nil {
			fmt.Print(code)
		}
	}
	copied := platform.Copy(rec.URL)
	kind := "file"
	if rec.Dir {
		kind = "folder"
	}
	facts := []string{kind + " " + tilde(rec.Path)}
	var later string
	checked = true
	if rec.Public {
		st := waitPublic(ctx, rec.ID)
		switch {
		case st.Public:
			facts = append(facts, "public")
		case st.PublicErr != "" && st.Reached == 0:
			facts = append(facts, "tailnet only for now")
			later = "not public yet: " + st.PublicErr
		case st.PublicErr != "":
			facts = append(facts, fmt.Sprintf("public through %d of %d addresses", st.Reached, st.Of))
			later = "some visitors cannot reach it: " + st.PublicErr
		default:
			checked = false
			if rec.Foreground {
				later = fmt.Sprintf("public through %d of %d addresses so far; this terminal says when all answer", st.Reached, st.Of)
			} else {
				record.MarkNotify(rec.ID)
				later = fmt.Sprintf("public through %d of %d addresses so far; a notification follows when all answer", st.Reached, st.Of)
			}
			if st.Of == 0 {
				later = strings.Replace(later, "public through 0 of 0 addresses so far", "public addresses not checked yet", 1)
			}
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
	if later != "" {
		fmt.Fprintln(os.Stderr, later)
	}
	return checked
}

// waitPublic follows the hub's check of a share's public addresses for up
// to announceWait and returns how far it got.
func waitPublic(ctx context.Context, id string) record.Status {
	var st record.Status
	deadline := time.Now().Add(announceWait)
	for time.Now().Before(deadline) {
		if h, err := record.LoadHub(); err == nil {
			st = h.Shares[id]
			if st.Public || (st.PublicErr != "" && st.Reached == 0) {
				return st
			}
		}
		select {
		case <-ctx.Done():
			return st
		case <-time.After(200 * time.Millisecond):
		}
	}
	return st
}

// follow prints a foreground share's requests until Ctrl+C, or until the
// share ends some other way (expiry, pagir stop). It reads from the share's
// creation, so requests made while announce waited show too.
func follow(ctx context.Context, rec *record.Share, checked bool) {
	fmt.Fprintln(os.Stderr, "Ctrl+C to stop. Requests:")
	lines := make(chan string)
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go tailLog(fctx, rec.ID, rec.Created, true, func(stamp time.Time, msg string) {
		select {
		case lines <- stamp.Format("15:04:05") + " " + msg:
		case <-fctx.Done():
		}
	})
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
			if !checked {
				h, err := record.LoadHub()
				if err != nil {
					continue
				}
				st := h.Shares[rec.ID]
				switch {
				case st.Public:
					checked = true
					fmt.Fprintf(os.Stderr, "%s public through all %d addresses\n", time.Now().Format("15:04:05"), st.Of)
				case st.PublicErr != "":
					checked = true
					fmt.Fprintf(os.Stderr, "%s public through %d of %d addresses; the check ended: %s\n", time.Now().Format("15:04:05"), st.Reached, st.Of, st.PublicErr)
				}
			}
		}
	}
}

func cmdList() error {
	recs, err := record.List()
	if err != nil {
		return err
	}
	h, _ := record.LoadHub()
	running := hubRunning()
	boot, knowBoot := platform.BootTime()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	n := 0
	for _, rec := range recs {
		if (knowBoot && rec.Created.Before(boot.Add(-bootSlack))) || (!running && time.Since(rec.Created) > 15*time.Second) {
			record.Remove(rec.ID) // nothing serves it any more
			continue
		}
		state := "starting"
		if st, ok := h.Shares[rec.ID]; ok && running {
			switch {
			case st.Error != "":
				state = "failing"
			case !st.Ready:
			case !rec.Public || st.Public:
				state = "live"
			case st.Of > 0:
				state = fmt.Sprintf("live, public %d/%d", st.Reached, st.Of)
			default:
				state = "live, checking public"
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
		return errors.New("stop which share? pagir stop ID..., pagir stop all, or pagir stop hub")
	}
	if len(args) == 1 && args[0] == "hub" {
		return stopHub()
	}
	var recs []*record.Share
	var missing []string
	if len(args) == 1 && args[0] == "all" {
		all, err := record.List()
		if err != nil {
			return err
		}
		recs = all
	} else {
		// One id that is already gone does not keep the rest running.
		for _, id := range args {
			rec, err := record.Load(id)
			if err != nil {
				missing = append(missing, id)
				continue
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
	if len(missing) > 0 {
		return fmt.Errorf("no share %s", strings.Join(missing, ", "))
	}
	return nil
}

// stopHub ends every share and the hub, which frees port 443.
func stopHub() error {
	recs, err := record.List()
	if err != nil {
		return err
	}
	for _, rec := range recs {
		record.Remove(rec.ID)
	}
	if !hubRunning() {
		fmt.Fprintln(os.Stderr, "no hub is running")
		return nil
	}
	if err := askHubToStop(); err != nil {
		return err
	}
	for range 200 {
		if !hubRunning() {
			fmt.Fprintf(os.Stderr, "stopped %d share(s) and the hub; port %d is free\n", len(recs), publicHTTPS)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the hub did not stop within 20s; the end of its log:\n%s", logTail(10))
}

// cmdLog shows one share's lines from hub.log.
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return tailLog(ctx, id, since, followFlag, func(stamp time.Time, msg string) {
		fmt.Println(stamp.Format("2006-01-02 15:04:05"), msg)
	})
}

// tailLog calls fn for each hub.log line about share id from since on,
// oldest first, reading hub.log.1 before hub.log. With follow it keeps
// reading new lines, across a rotation, until ctx ends.
func tailLog(ctx context.Context, id string, since time.Time, follow bool, fn func(time.Time, string)) error {
	p, err := record.Path("hub.log")
	if err != nil {
		return err
	}
	emit := func(line string) {
		if len(line) <= len(logStamp) {
			return
		}
		stamp, err := time.ParseInLocation(logStamp, line[:len(logStamp)], time.Local)
		if err != nil || stamp.Before(since.Add(-time.Second)) {
			return
		}
		if msg, ok := strings.CutPrefix(line[len(logStamp)+1:], id+" "); ok {
			fn(stamp, msg)
		}
	}
	if old, err := os.Open(p + ".1"); err == nil {
		sc := bufio.NewScanner(old)
		for sc.Scan() {
			emit(sc.Text())
		}
		old.Close()
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) && !follow {
		return nil
	}
	for err != nil {
		if !follow {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		f, err = os.Open(p)
	}
	defer func() { f.Close() }()
	r := bufio.NewReader(f)
	var partial string
	for {
		chunk, err := r.ReadString('\n')
		partial += chunk
		if err == nil {
			emit(strings.TrimRight(partial, "\r\n"))
			partial = ""
			continue
		}
		if !follow {
			if partial != "" {
				emit(partial)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
		// A new supervisor moves hub.log aside past logLimit: follow the new one.
		if cur, err1 := f.Stat(); err1 == nil {
			if now, err2 := os.Stat(p); err2 == nil && !os.SameFile(cur, now) {
				if nf, err3 := os.Open(p); err3 == nil {
					f.Close()
					f, r, partial = nf, bufio.NewReader(nf), ""
				}
			}
		}
	}
}

// logTail is the last n lines of hub.log, for error messages.
func logTail(n int) string {
	p, err := record.Path("hub.log")
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "(no hub.log)"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
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

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
