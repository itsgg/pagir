// Package web serves one shared file or folder under a secret path prefix.
//
// A folder is opened as an os.Root, so every open, stat and create is
// confined to it: a ".." segment or a symlink pointing outside fails rather
// than escaping. Dotfiles are absent from listings and zips and refused by
// name unless Hidden is set.
package web

import (
	"archive/zip"
	"compress/flate"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Config describes one share.
type Config struct {
	Root     string // absolute path of the shared file or folder
	Prefix   string // "/<token>", no trailing slash
	Upload   bool   // folder shares: accept uploads
	Hidden   bool   // serve dotfiles
	Password string // non-empty: HTTP basic auth with this password, any user
	Log      *log.Logger
	Unlogged func(*http.Request) bool // requests to leave out of Log, such as pagir's own checks
}

// Server is an http.Handler for one share. Close releases the folder handle.
type Server struct {
	c    Config
	root *os.Root // nil for a file share
	name string   // base name of Root
}

// New checks that Root exists and opens it.
func New(c Config) (*Server, error) {
	if !strings.HasPrefix(c.Prefix, "/") || strings.HasSuffix(c.Prefix, "/") {
		return nil, fmt.Errorf("bad prefix %q", c.Prefix)
	}
	fi, err := os.Stat(c.Root)
	if err != nil {
		return nil, err
	}
	s := &Server{c: c, name: filepath.Base(c.Root)}
	if fi.IsDir() {
		if s.root, err = os.OpenRoot(c.Root); err != nil {
			return nil, err
		}
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is neither a file nor a folder", c.Root)
	} else if c.Upload {
		return nil, errors.New("uploads need a folder, not a file")
	}
	return s, nil
}

// Close releases the folder handle.
func (s *Server) Close() error {
	if s.root != nil {
		return s.root.Close()
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	lw := &logWriter{ResponseWriter: w, status: http.StatusOK}
	s.serve(lw, r)
	if s.c.Log != nil && (s.c.Unlogged == nil || !s.c.Unlogged(r)) {
		who := r.Header.Get("X-Forwarded-For")
		if who == "" {
			who = r.RemoteAddr
		}
		if login := r.Header.Get("Tailscale-User-Login"); login != "" {
			who += " (" + login + ")"
		}
		target := s.public(r.URL.Path)
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		s.c.Log.Printf("%s %s %s %d %dB %s", who, r.Method, target, lw.status, lw.bytes, time.Since(start).Round(time.Millisecond))
	}
}

// public drops the token from a logged path, so the log is safe to paste.
func (s *Server) public(p string) string {
	if rest, ok := strings.CutPrefix(p, s.c.Prefix); ok {
		return "/..." + rest
	}
	return p
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	p := r.URL.Path
	if p == s.c.Prefix {
		redirect(w, r, s.c.Prefix+"/")
		return
	}
	rel, ok := strings.CutPrefix(p, s.c.Prefix+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if s.c.Password != "" {
		_, pw, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(s.c.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="pagir", charset="UTF-8"`)
			http.Error(w, "password required", http.StatusUnauthorized)
			return
		}
	}
	if s.root == nil {
		s.serveSingle(w, r, rel)
		return
	}
	s.serveTree(w, r, rel)
}

func (s *Server) serveSingle(w http.ResponseWriter, r *http.Request, rel string) {
	switch rel {
	case "":
		redirect(w, r, s.c.Prefix+"/"+url.PathEscape(s.name))
	case s.name:
		if !getOrHead(w, r) {
			return
		}
		f, err := os.Open(s.c.Root)
		if err != nil {
			http.Error(w, "file is gone", http.StatusNotFound)
			return
		}
		defer f.Close()
		s.serveFile(w, r, f, s.name)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveTree(w http.ResponseWriter, r *http.Request, rel string) {
	if strings.ContainsRune(rel, 0) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	name := cleanRel(rel)
	if !s.c.Hidden && hasDotSegment(name) {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPut {
		s.put(w, r, name)
		return
	}
	fi, err := s.root.Stat(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if fi.IsDir() {
		if rel != "" && !strings.HasSuffix(rel, "/") {
			redirect(w, r, r.URL.EscapedPath()+"/")
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			if _, ok := r.URL.Query()["zip"]; ok {
				s.zip(w, r, name)
			} else {
				s.list(w, r, name)
			}
		case http.MethodPost:
			s.post(w, r, name)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	if !getOrHead(w, r) {
		return
	}
	f, err := s.root.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	s.serveFile(w, r, f, path.Base(name))
}

// serveFile uses ServeContent for range requests and conditional GETs.
// ?dl asks the browser to save rather than display.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, f *os.File, name string) {
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "cannot read file", http.StatusInternalServerError)
		return
	}
	disp := "inline"
	if _, ok := r.URL.Query()["dl"]; ok {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// zip streams the folder at name as one archive. An error after the first
// byte cannot change the status, so it aborts the connection instead, and
// the client sees a failed download rather than a short archive that looks whole.
func (s *Server) zip(w http.ResponseWriter, r *http.Request, name string) {
	base := s.name
	if name != "." {
		base = path.Base(name)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": base + ".zip"}))
	if r.Method == http.MethodHead {
		return
	}
	fsys := s.root.FS()
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	err := fs.WalkDir(fsys, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Leave out what cannot be read, as the listing does; only the
			// folder asked for failing ends the download.
			if p == name {
				return err
			}
			if s.c.Log != nil {
				s.c.Log.Printf("zip skips %s: %v", p, err)
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p != name && !s.c.Hidden && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Stat follows a symlink, still confined to the root; anything that
		// is not a regular file at the end of it is left out.
		fi, err := fs.Stat(fsys, p)
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, name), "/")
		if name == "." {
			rel = p
		}
		f, err := fsys.Open(p)
		if err != nil {
			if s.c.Log != nil {
				s.c.Log.Printf("zip skips %s: %v", p, err)
			}
			return nil
		}
		defer f.Close()
		hw, err := zw.CreateHeader(&zip.FileHeader{Name: base + "/" + rel, Method: zip.Deflate, Modified: fi.ModTime()})
		if err != nil {
			return err
		}
		_, err = io.Copy(hw, f)
		return err
	})
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		if s.c.Log != nil {
			s.c.Log.Printf("zip %s: %v", s.public(r.URL.Path), err)
		}
		panic(http.ErrAbortHandler)
	}
}

// post takes a multipart form from the folder page; each file lands under a
// name no existing file has.
func (s *Server) post(w http.ResponseWriter, r *http.Request, dir string) {
	if !s.c.Upload {
		http.Error(w, "uploads are off for this share", http.StatusForbidden)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected a multipart form", http.StatusBadRequest)
		return
	}
	var saved []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "upload interrupted", http.StatusBadRequest)
			return
		}
		if part.FileName() == "" {
			continue
		}
		got, err := s.store(dir, part.FileName(), part)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		saved = append(saved, got)
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, r.URL.EscapedPath(), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	for _, n := range saved {
		fmt.Fprintln(w, n)
	}
}

// put takes `curl -T file URL/`, which curl sends as PUT URL/file.
func (s *Server) put(w http.ResponseWriter, r *http.Request, name string) {
	if !s.c.Upload {
		http.Error(w, "uploads are off for this share", http.StatusForbidden)
		return
	}
	dir := path.Dir(name)
	if fi, err := s.root.Stat(dir); err != nil || !fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	got, err := s.store(dir, path.Base(name), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintln(w, got)
}

// store writes src into dir under filename, or "name (n).ext" when taken,
// and never overwrites. A failed copy leaves no partial file behind.
func (s *Server) store(dir, filename string, src io.Reader) (string, error) {
	filename = path.Base(strings.ReplaceAll(filename, `\`, "/"))
	if filename == "." || filename == "/" || filename == ".." || filename == "" {
		return "", errors.New("bad file name")
	}
	if !s.c.Hidden && strings.HasPrefix(filename, ".") {
		return "", errors.New("dotfiles are hidden on this share")
	}
	ext := path.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	for n := 0; n < 1000; n++ {
		cand := filename
		if n > 0 {
			cand = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		target := path.Join(dir, cand)
		f, err := s.root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", errors.New("cannot create " + cand)
		}
		_, err = io.Copy(f, src)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			s.root.Remove(target)
			return "", errors.New("upload of " + cand + " failed")
		}
		if s.c.Log != nil {
			s.c.Log.Printf("uploaded %s", target)
		}
		return cand, nil
	}
	return "", errors.New("too many files named " + filename)
}

type entry struct {
	Name, Href, Size, Modified string
	Dir                        bool
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, name string) {
	f, err := s.root.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil {
		http.Error(w, "cannot read folder", http.StatusInternalServerError)
		return
	}
	var entries []entry
	for _, d := range des {
		if !s.c.Hidden && strings.HasPrefix(d.Name(), ".") {
			continue
		}
		fi, err := s.root.Stat(path.Join(name, d.Name()))
		if err != nil {
			continue // a dangling link, or one that escapes the root
		}
		e := entry{Name: d.Name(), Href: "./" + url.PathEscape(d.Name()), Modified: fi.ModTime().Format("2006-01-02 15:04"), Dir: fi.IsDir()}
		if e.Dir {
			e.Name += "/"
			e.Href += "/"
		} else {
			e.Size = humanSize(fi.Size())
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	title := s.name
	if name != "." {
		title = s.name + "/" + name
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		return
	}
	err = listing.Execute(w, map[string]any{
		"Title":   title,
		"Up":      name != ".",
		"Entries": entries,
		"Upload":  s.c.Upload,
	})
	if err != nil && s.c.Log != nil {
		s.c.Log.Printf("listing: %v", err)
	}
}

// cleanRel turns the part of a URL after the prefix into an os.Root name.
func cleanRel(rel string) string {
	c := strings.TrimPrefix(path.Clean("/"+rel), "/")
	if c == "" {
		return "."
	}
	return c
}

func hasDotSegment(name string) bool {
	for seg := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." {
			return true
		}
	}
	return false
}

func getOrHead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

// redirect keeps the query string, so ?zip and ?dl survive a slash redirect.
func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, to, http.StatusFound)
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

type logWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (l *logWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}

func (l *logWriter) Write(b []byte) (int, error) {
	n, err := l.ResponseWriter.Write(b)
	l.bytes += int64(n)
	return n, err
}

// ReadFrom keeps the sendfile path open for ServeContent.
func (l *logWriter) ReadFrom(src io.Reader) (int64, error) {
	n, err := io.Copy(l.ResponseWriter, src)
	l.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer.
func (l *logWriter) Unwrap() http.ResponseWriter { return l.ResponseWriter }
