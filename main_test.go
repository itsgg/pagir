package main

import (
	"context"
	"flag"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/itsgg/pagir/internal/record"
)

func TestParseLifetime(t *testing.T) {
	good := map[string]time.Duration{"30m": 30 * time.Minute, "2h": 2 * time.Hour, "1h30m": 90 * time.Minute, "3d": 72 * time.Hour, "1w": 168 * time.Hour, "1.5d": 36 * time.Hour}
	for in, want := range good {
		if d, never, err := parseLifetime(in); err != nil || never || d != want {
			t.Errorf("%s: %v %v %v, want %v", in, d, never, err, want)
		}
	}
	if _, never, err := parseLifetime("never"); err != nil || !never {
		t.Error("never")
	}
	for _, bad := range []string{"", "0", "-1h", "xd", "soon", "0d"} {
		if _, _, err := parseLifetime(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseInterleaved(t *testing.T) {
	var e string
	var u bool
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.StringVar(&e, "e", "", "")
	fs.BoolVar(&u, "u", false, "")
	pos, err := parseInterleaved(fs, []string{"dir", "-e", "2h", "-u", "--", "-weird"})
	if err != nil || !slices.Equal(pos, []string{"dir", "-weird"}) || e != "2h" || !u {
		t.Fatalf("pos %v e %q u %v err %v", pos, e, u, err)
	}
}

func TestFmtLeft(t *testing.T) {
	for d, want := range map[time.Duration]string{50 * time.Hour: "2d2h", 90 * time.Minute: "1h30m", 20 * time.Second: "1m", -time.Second: "ending"} {
		if got := fmtLeft(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func TestTailLog(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p, err := record.Path("hub.log")
	if err != nil {
		t.Fatal(err)
	}
	at := func(d time.Duration, msg string) string {
		return time.Now().Add(d).Format(logStamp) + " " + msg + "\n"
	}
	os.WriteFile(p+".1", []byte(at(-time.Hour, "aaaaaa old line")+at(-10*time.Second, "aaaaaa rotated")), 0o600)
	os.WriteFile(p, []byte(at(-5*time.Second, "bbbbbb other share")+at(-4*time.Second, "aaaaaa GET /.../x 200")+"garbage\n"+at(-3*time.Second, "aaaaaab not this one")), 0o600)
	var got []string
	err = tailLog(context.Background(), "aaaaaa", time.Now().Add(-time.Minute), false, func(_ time.Time, msg string) {
		got = append(got, msg)
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"rotated", "GET /.../x 200"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestShareLoggerFeedsTailLog holds the hub's writer and the reader together.
func TestShareLoggerFeedsTailLog(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p, err := record.Path("hub.log")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	shareLogger(f, "abc123").Printf("GET /.../x 200")
	newLogger(f).Printf("abc123 expired")
	f.Close()
	var got []string
	tailLog(context.Background(), "abc123", time.Now().Add(-time.Minute), false, func(_ time.Time, msg string) {
		got = append(got, msg)
	})
	if want := []string{"GET /.../x 200", "expired"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
