package web

import (
	"archive/zip"
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const prefix = "/tok"

// tree makes a shared folder plus a secret file beside it, outside the share.
func tree(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "share")
	outside = filepath.Join(base, "secret.txt")
	write(t, outside, "secret")
	write(t, filepath.Join(root, "a.txt"), "hello world")
	write(t, filepath.Join(root, "sub", "b.txt"), "inner")
	write(t, filepath.Join(root, ".env"), "TOKEN=1")
	write(t, filepath.Join(root, ".git", "config"), "[core]")
	write(t, filepath.Join(root, "a b#c.txt"), "odd name")
	if err := os.Symlink(outside, filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "inside.txt")); err != nil {
		t.Fatal(err)
	}
	// os.Root follows no absolute link, even one that points inside.
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "absolute.txt")); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func server(t *testing.T, c Config) *Server {
	t.Helper()
	if c.Prefix == "" {
		c.Prefix = prefix
	}
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func do(s http.Handler, method, target string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://x"+prefix, body)
	r.URL.Path = target // raw, so ".." reaches the handler the way a hostile client sends it
	r.URL.RawPath = ""
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestFolderServesFilesAndRefusesEscapes(t *testing.T) {
	root, _ := tree(t)
	s := server(t, Config{Root: root})
	cases := []struct {
		path   string
		status int
		body   string
	}{
		{"/tok/a.txt", 200, "hello world"},
		{"/tok/sub/b.txt", 200, "inner"},
		{"/tok/a b#c.txt", 200, "odd name"},
		{"/tok/inside.txt", 200, "hello world"},
		{"/tok/escape.txt", 404, ""},
		{"/tok/absolute.txt", 404, ""},
		{"/tok/../secret.txt", 404, ""},
		{"/tok/sub/../../secret.txt", 404, ""},
		{"/tok/.env", 404, ""},
		{"/tok/.git/config", 404, ""},
		{"/tok/missing", 404, ""},
		{"/other/a.txt", 404, ""},
		{"/tokx/a.txt", 404, ""},
	}
	for _, c := range cases {
		w := do(s, "GET", c.path, nil)
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d", c.path, w.Code, c.status)
		}
		if c.body != "" && w.Body.String() != c.body {
			t.Errorf("%s: body %q, want %q", c.path, w.Body.String(), c.body)
		}
		if strings.Contains(w.Body.String(), "secret") {
			t.Errorf("%s: leaked the file outside the share", c.path)
		}
	}
}

func TestHiddenServesDotfiles(t *testing.T) {
	root, _ := tree(t)
	s := server(t, Config{Root: root, Hidden: true})
	if w := do(s, "GET", "/tok/.env", nil); w.Code != 200 || w.Body.String() != "TOKEN=1" {
		t.Fatalf(".env with Hidden: %d %q", w.Code, w.Body.String())
	}
	if w := do(s, "GET", "/tok/", nil); !strings.Contains(w.Body.String(), ".env") {
		t.Error("listing with Hidden leaves out .env")
	}
}

func TestListing(t *testing.T) {
	root, _ := tree(t)
	s := server(t, Config{Root: root})
	w := do(s, "GET", "/tok/", nil)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`href="./a.txt"`, `href="./sub/"`, `href="./a%20b%23c.txt"`, `href="?zip"`} {
		if !strings.Contains(body, want) {
			t.Errorf("listing lacks %s", want)
		}
	}
	for _, unwanted := range []string{".env", ".git", "escape.txt", "<form"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("listing shows %s", unwanted)
		}
	}
	if w := do(s, "GET", "/tok/sub", nil); w.Code != http.StatusFound || w.Header().Get("Location") != "/tok/sub/" {
		t.Errorf("folder without slash: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if w := do(s, "GET", "/tok", nil); w.Code != http.StatusFound || w.Header().Get("Location") != "/tok/" {
		t.Errorf("bare prefix: %d to %q", w.Code, w.Header().Get("Location"))
	}
}

func TestRange(t *testing.T) {
	root, _ := tree(t)
	s := server(t, Config{Root: root})
	w := do(s, "GET", "/tok/a.txt", nil, "Range", "bytes=0-4")
	if w.Code != http.StatusPartialContent || w.Body.String() != "hello" {
		t.Fatalf("range: %d %q", w.Code, w.Body.String())
	}
}

func TestZip(t *testing.T) {
	root, _ := tree(t)
	s := server(t, Config{Root: root})
	r := httptest.NewRequest("GET", "http://x/tok/?zip", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	slices.Sort(names)
	want := []string{"share/a b#c.txt", "share/a.txt", "share/inside.txt", "share/sub/b.txt"}
	if !slices.Equal(names, want) {
		t.Fatalf("zip holds %v, want %v", names, want)
	}
}

func TestZipSkipsWhatItCannotRead(t *testing.T) {
	root, _ := tree(t)
	write(t, filepath.Join(root, "locked", "x.txt"), "x")
	write(t, filepath.Join(root, "unreadable.txt"), "y")
	for _, p := range []string{filepath.Join(root, "locked"), filepath.Join(root, "unreadable.txt")} {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "locked"), 0o755) })
	if f, err := os.Open(filepath.Join(root, "unreadable.txt")); err == nil {
		f.Close()
		t.Skip("running as root, so nothing is unreadable")
	}
	s := server(t, Config{Root: root})
	r := httptest.NewRequest("GET", "http://x/tok/?zip", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("zip with an unreadable folder inside: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if !slices.Contains(names, "share/a.txt") || slices.Contains(names, "share/unreadable.txt") || slices.Contains(names, "share/locked/x.txt") {
		t.Errorf("zip holds %v", names)
	}
}

func TestUploadOffByDefault(t *testing.T) {
	root, _ := tree(t)
	s := server(t, Config{Root: root})
	if w := do(s, "PUT", "/tok/new.txt", strings.NewReader("x")); w.Code != http.StatusForbidden {
		t.Errorf("PUT without Upload: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); err == nil {
		t.Error("PUT without Upload wrote a file")
	}
}

func TestUploadNeverOverwrites(t *testing.T) {
	root, _ := tree(t)
	s := server(t, Config{Root: root, Upload: true})
	if w := do(s, "PUT", "/tok/a.txt", strings.NewReader("new")); w.Code != http.StatusCreated || strings.TrimSpace(w.Body.String()) != "a (1).txt" {
		t.Fatalf("PUT over a.txt: %d %q", w.Code, w.Body.String())
	}
	if got, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(got) != "hello world" {
		t.Error("PUT overwrote a.txt")
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "../../evil.txt")
	fw.Write([]byte("form"))
	mw.Close()
	w := do(s, "POST", "/tok/sub/", &body, "Content-Type", mw.FormDataContentType())
	if w.Code != http.StatusCreated {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(root, "sub", "evil.txt")); err != nil || string(got) != "form" {
		t.Errorf("upload did not land in sub/ under its base name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil.txt")); err == nil {
		t.Error("upload escaped the share")
	}
	if w := do(s, "PUT", "/tok/.bashrc", strings.NewReader("x")); w.Code != http.StatusNotFound {
		t.Errorf("PUT of a dotfile: %d", w.Code)
	}
	if w := do(s, "GET", "/tok/", nil); !strings.Contains(w.Body.String(), "<form") {
		t.Error("listing with Upload has no form")
	}
}

func TestPassword(t *testing.T) {
	root, _ := tree(t)
	s := server(t, Config{Root: root, Password: "pw"})
	if w := do(s, "GET", "/tok/a.txt", nil); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no password: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "http://x/tok/a.txt", nil)
	r.SetBasicAuth("anyone", "wrong")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", w.Code)
	}
	r.SetBasicAuth("anyone", "pw")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("right password: %d", w.Code)
	}
}

func TestSingleFile(t *testing.T) {
	root, _ := tree(t)
	file := filepath.Join(root, "a b#c.txt")
	s := server(t, Config{Root: file})
	if w := do(s, "GET", "/tok/", nil); w.Code != http.StatusFound || w.Header().Get("Location") != "/tok/a%20b%23c.txt" {
		t.Errorf("root: %d to %q", w.Code, w.Header().Get("Location"))
	}
	w := do(s, "GET", "/tok/a b#c.txt", nil)
	if w.Code != 200 || w.Body.String() != "odd name" {
		t.Fatalf("file: %d %q", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Errorf("Content-Disposition %q", cd)
	}
	for _, p := range []string{"/tok/a.txt", "/tok/../a.txt", "/tok/a b#c.txt/x"} {
		if w := do(s, "GET", p, nil); w.Code != 404 {
			t.Errorf("%s: %d, want 404", p, w.Code)
		}
	}
	if _, err := New(Config{Root: file, Prefix: prefix, Upload: true}); err == nil {
		t.Error("Upload on a file share was accepted")
	}
}
