package mib

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Repo is a git repository of MIB files.
type Repo struct {
	Name string
	URL  string
	Ref  string
	// Paths narrows the checkout to the directories that hold MIB files.
	// Empty means the whole tree.
	Paths []string
}

// DefaultRepos are the public collections that between them cover most
// network equipment.
//
// librenms/librenms carries the same files under mibs/ but is thirty times
// larger because of the application around them, so the standalone mirror is
// the one worth cloning.
var DefaultRepos = []Repo{
	{Name: "librenms", URL: "https://github.com/librenms/librenms-mibs.git", Ref: "master"},
	// Cisco's repository is mostly not MIBs: supportlists/, schema/ and oid/
	// are derived matrices that together outweigh the definitions and index to
	// nothing. Checking them out would triple the cache for no lookups.
	{Name: "cisco", URL: "https://github.com/cisco/cisco-mibs.git", Ref: "main",
		Paths: []string{"v2", "v1", "sdwan", "traps", "ucs-mibs", "ucs-C-Series-mibs", "ME1200-MIBS"}},
}

// RepoByName finds a default repo. A name that is not one of them is treated
// as a URL, so a vendor's own collection can be added without a config file.
func RepoByName(nameOrURL string) Repo {
	for _, r := range DefaultRepos {
		if r.Name == nameOrURL {
			return r
		}
	}
	name := strings.TrimSuffix(filepath.Base(nameOrURL), ".git")
	return Repo{Name: name, URL: nameOrURL, Ref: ""}
}

// indexName is the generated lookup table. It is plain TSV on purpose: the
// CLI reads it, and so can grep, which matters when an agent wants a subtree
// the search flags do not express.
const indexName = "index.tsv"

const indexHeader = "#ups-mib-index\tv1"

// Store is the local MIB cache.
type Store struct{ Dir string }

// DefaultDir honours UPS_MIB_DIR, then XDG_CACHE_HOME, then ~/.cache/ups/mibs.
func DefaultDir() (string, error) {
	if d := os.Getenv("UPS_MIB_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "ups", "mibs"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".cache", "ups", "mibs"), nil
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) IndexPath() string        { return filepath.Join(s.Dir, indexName) }
func (s *Store) RepoPath(n string) string { return filepath.Join(s.Dir, "repos", n) }

// RepoState is what is on disk for one repository.
type RepoState struct {
	Name     string
	URL      string
	Revision string
	Synced   time.Time
}

// Fetch clones or updates one repository.
//
// The clone is shallow and single-branch: these repositories are large and
// their history is of no use here, only the current files are.
func (s *Store) Fetch(r Repo, progress io.Writer) (RepoState, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return RepoState{}, fmt.Errorf("git is required to sync MIB repositories but was not found on PATH")
	}
	dir := s.RepoPath(r.Name)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return RepoState{}, err
	}

	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		// Reset rather than merge: the working tree is a cache, never edited,
		// and a merge conflict here would be a dead end with no useful fix.
		ref := r.Ref
		if ref == "" {
			ref = "HEAD"
		}
		if err := run(progress, "git", "-C", dir, "fetch", "--depth", "1", "origin", ref); err != nil {
			return RepoState{}, err
		}
		if err := run(progress, "git", "-C", dir, "reset", "--hard", "FETCH_HEAD"); err != nil {
			return RepoState{}, err
		}
	} else {
		args := []string{"clone", "--depth", "1", "--single-branch"}
		if r.Ref != "" {
			args = append(args, "--branch", r.Ref)
		}
		if len(r.Paths) > 0 {
			// A partial, sparse clone: the blobs outside the cone are never
			// transferred, so this is a smaller download as well as a smaller
			// checkout.
			args = append(args, "--filter=blob:none", "--sparse")
		}
		args = append(args, r.URL, dir)
		if err := run(progress, "git", args...); err != nil {
			return RepoState{}, err
		}
		if len(r.Paths) > 0 {
			set := append([]string{"-C", dir, "sparse-checkout", "set", "--cone"}, r.Paths...)
			if err := run(progress, "git", set...); err != nil {
				return RepoState{}, err
			}
		}
	}
	return s.stateOf(r.Name)
}

func (s *Store) stateOf(name string) (RepoState, error) {
	dir := s.RepoPath(name)
	st := RepoState{Name: name}
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		st.Synced = fi.ModTime()
	} else {
		return st, fmt.Errorf("%s is not cached", name)
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output(); err == nil {
		st.Revision = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output(); err == nil {
		st.URL = strings.TrimSpace(string(out))
	}
	return st, nil
}

// Cached lists the repositories present on disk.
func (s *Store) Cached() []RepoState {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "repos"))
	if err != nil {
		return nil
	}
	var out []RepoState
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if st, err := s.stateOf(e.Name()); err == nil {
			out = append(out, st)
		}
	}
	return out
}

// skipExt are files the repositories carry alongside the MIBs. Parsing them
// costs time and finds nothing.
var skipExt = map[string]bool{
	".md": true, ".json": true, ".yml": true, ".yaml": true, ".php": true,
	".py": true, ".pl": true, ".sh": true, ".png": true, ".jpg": true,
	".gif": true, ".pdf": true, ".zip": true, ".gz": true, ".tgz": true,
	".xml": true, ".html": true, ".css": true, ".js": true, ".sql": true,
}

// maxFileSize skips anything too large to be a MIB. The largest real ones are
// a couple of megabytes.
const maxFileSize = 16 << 20

// Reindex parses every cached MIB and rewrites the index.
func (s *Store) Reindex() (int, error) {
	root := filepath.Join(s.Dir, "repos")
	var objs []Object

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner of the cache is not fatal
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if skipExt[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Size() > maxFileSize {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		_, found := ParseFile(path, filepath.ToSlash(rel))
		objs = append(objs, found...)
		return nil
	})
	if err != nil {
		return 0, err
	}

	objs = Resolve(objs)
	objs = dedupe(objs)
	SortIndex(objs)
	return len(objs), s.writeIndex(objs)
}

// dedupe collapses the copies of the same object.
//
// The repositories carry standard MIBs several times over, once per vendor
// directory that needed them, so IF-MIB alone appears a dozen times. Listing
// a subtree would otherwise repeat every row and bury the differences that
// matter. Two objects are the same when the name and the resolved OID match;
// a name reused at a different OID by another vendor is a real collision and
// both are kept.
func dedupe(objs []Object) []Object {
	at := make(map[string]int, len(objs))
	out := objs[:0]
	for _, o := range objs {
		k := o.Name + "\x00" + o.OID
		i, seen := at[k]
		if !seen {
			at[k] = len(out)
			out = append(out, o)
			continue
		}
		// Not all copies are equal. Cisco ships an auto-converted SMIv1 tree
		// beside the real MIBs, where entries read
		// `SYNTAX --?? syntax is not convertable to SMIv1`; keeping that one
		// because it was walked first would report an object as having no
		// syntax when the authoritative definition says Counter64.
		if detail(o) > detail(out[i]) {
			out[i] = o
		}
	}
	return out
}

// detail scores how much of a definition survived, for picking between copies.
func detail(o Object) int {
	n := 0
	if o.Syntax != "" {
		n += 2
	}
	if o.Description != "" {
		n++
	}
	if o.Access != "" {
		n++
	}
	return n
}

func (s *Store) writeIndex(objs []Object) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	tmp := s.IndexPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	fmt.Fprintln(w, indexHeader)
	for _, o := range objs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			o.Name, o.OID, o.Kind, clean(o.Syntax), clean(o.Access),
			o.Module, o.File, clean(truncate(o.Description, 240)))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.IndexPath())
}

func clean(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.ReplaceAll(s, "\n", " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func run(progress io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var sink strings.Builder
	cmd.Stdout = &sink
	cmd.Stderr = &sink
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(sink.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	if progress != nil && sink.Len() > 0 {
		fmt.Fprintln(progress, strings.TrimSpace(sink.String()))
	}
	return nil
}
