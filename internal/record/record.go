// Package record keeps the state pagir shares between processes, under
// $XDG_STATE_HOME/pagir: one JSON file per share, which only the CLI writes,
// and hub.json, which only the hub writes. The hub may delete a share's file
// (expiry, a foreground owner gone) but never writes one, so a share the CLI
// stopped cannot be brought back by a late write.
package record

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Share is everything one share needs to run and to be listed.
type Share struct {
	ID         string    `json:"id"`
	Token      string    `json:"token"`
	Path       string    `json:"path"`
	Dir        bool      `json:"dir"`
	Public     bool      `json:"public"`
	Upload     bool      `json:"upload,omitempty"`
	Hidden     bool      `json:"hidden,omitempty"`
	Password   string    `json:"password,omitempty"`
	URL        string    `json:"url"`
	Created    time.Time `json:"created"`
	Expires    time.Time `json:"expires,omitzero"`
	Foreground bool      `json:"foreground,omitempty"`
	Owner      int       `json:"owner,omitempty"` // pid of the attached CLI of a foreground share
	Boot       string    `json:"boot"`            // the boot it was made in; a share never outlives a reboot
}

// CurrentBoot is the kernel's id for this boot.
func CurrentBoot() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

// Expired reports whether the share's lifetime is over at now.
func (s *Share) Expired(now time.Time) bool { return !s.Expires.IsZero() && !now.Before(s.Expires) }

// Mount is the path tailscaled serves this share under.
func (s *Share) Mount() string { return "/" + s.Token }

var idPattern = regexp.MustCompile(`^[0-9a-f]{6}$`)

// ValidID reports whether id has the shape NewID makes, so a user-supplied id
// can never name a file outside the state directory.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Dir is the state directory, created on first use.
func Dir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "pagir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// File is the record file for id.
func File(id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("no share %q", id)
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

// Create gives s a fresh id and writes its record. The file is linked into
// place, which fails rather than replaces when the id is taken, so two
// pagirs drawing the same id cannot overwrite each other.
func Create(s *Share) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	for range 100 {
		b := make([]byte, 3)
		rand.Read(b)
		s.ID = hex.EncodeToString(b)
		tmp, err := writeTemp(dir, s)
		if err != nil {
			return err
		}
		err = os.Link(tmp, filepath.Join(dir, s.ID+".json"))
		os.Remove(tmp)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return err
	}
	return errors.New("no free share id")
}

// NewToken returns 128 random bits as lowercase base32, the secret part of a URL.
func NewToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// Save writes s atomically, readable only by the user since it can hold a password.
func Save(s *Share) error {
	file, err := File(s.ID)
	if err != nil {
		return err
	}
	return SaveTo(file, s)
}

// SaveTo writes s to an explicit file.
func SaveTo(file string, s *Share) error { return writeJSON(file, s) }

func writeJSON(file string, v any) error {
	tmp, err := writeTemp(filepath.Dir(file), v)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, file)
}

// writeTemp writes v to a hidden file in dir, outside List's glob.
func writeTemp(dir string, v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// Load reads the record for id.
func Load(id string) (*Share, error) {
	file, err := File(id)
	if err != nil {
		return nil, err
	}
	s, err := LoadFrom(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no share %q", id)
	}
	return s, err
}

// LoadFrom reads a record from an explicit file.
func LoadFrom(file string) (*Share, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var s Share
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &s, nil
}

// List returns every record, oldest first. A file that does not parse is
// skipped rather than failing the listing.
func List() ([]*Share, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Share
	for _, f := range files {
		if s, err := LoadFrom(f); err == nil && ValidID(s.ID) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// Remove deletes the record for id; a missing record is not an error.
func Remove(id string) error {
	file, err := File(id)
	if err != nil {
		return err
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Status is the hub's view of one share.
type Status struct {
	Ready bool   `json:"ready,omitempty"`
	Error string `json:"error,omitempty"`
}

// Hub is what the hub publishes about itself: its local ports, which tell
// pagir's tailscale mounts from anyone else's, and each share's status.
type Hub struct {
	PID         int               `json:"pid"`
	PublicPort  int               `json:"public_port"`
	TailnetPort int               `json:"tailnet_port"`
	Shares      map[string]Status `json:"shares"`
	Updated     time.Time         `json:"updated"`
}

func hubFile() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub.json"), nil
}

// SaveHub writes hub.json; only the hub calls it.
func SaveHub(h *Hub) error {
	file, err := hubFile()
	if err != nil {
		return err
	}
	return writeJSON(file, h)
}

// LoadHub reads hub.json; a missing file is an empty hub.
func LoadHub() (*Hub, error) {
	file, err := hubFile()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return &Hub{}, nil
	}
	if err != nil {
		return nil, err
	}
	var h Hub
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &h, nil
}

// RemoveHub deletes hub.json when the hub exits.
func RemoveHub() {
	if file, err := hubFile(); err == nil {
		os.Remove(file)
	}
}
