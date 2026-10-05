package main

import (
	"flag"
	"slices"
	"testing"
	"time"
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
