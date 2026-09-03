package iac

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A directory export is laid out so that git diffs stay readable: one file per
// host, plus a manifest naming the infrastructure.
//
//	infra/
//	  infrastructure.yaml
//	  templates/
//	    cisco-ios-switch.yaml
//	  hosts/
//	    core-sw-01.yaml
//	    fw-01.yaml
//
// Templates get their own files for the reason the split exists at all: one
// template is referenced by many hosts, so editing a check should be a one-line
// diff in one file rather than the same edit repeated across every host that
// carries it.
//
// The manifest is what marks a directory as an export; without it, Load
// refuses rather than guessing at arbitrary YAML it finds.
const (
	ManifestName = "infrastructure.yaml"
	hostsDir     = "hosts"
	templatesDir = "templates"
)

// manifest is the directory-mode header file.
type manifest struct {
	APIVersion     string          `yaml:"apiVersion"`
	Infrastructure InfrastructureR `yaml:"infrastructure"`
}

// SaveResult reports what a write touched, so the CLI can show it.
type SaveResult struct {
	Written []string
	// Removed lists host files pruned because the resource no longer exists
	// remotely. Leaving them would make the next diff propose re-creating
	// hosts that were deleted on purpose.
	Removed   []string
	Directory bool
}

// IsDirTarget reports whether a path should be written as a directory: either
// it already is one, or it was written with a trailing separator.
func IsDirTarget(path string) bool {
	if path == "" || path == "-" {
		return false
	}
	if strings.HasSuffix(path, string(os.PathSeparator)) || strings.HasSuffix(path, "/") {
		return true
	}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		return true
	}
	// A path with no extension is treated as a directory: `--out ./infra` is
	// far more likely to mean a folder than a YAML file named "infra".
	return filepath.Ext(path) == ""
}

// Marshal renders a document as a single YAML file.
func Marshal(doc *Document) ([]byte, error) {
	doc.Normalize()
	return yaml.Marshal(doc)
}

// Save writes a document to a file or a directory, depending on the path.
func Save(doc *Document, path string) (*SaveResult, error) {
	doc.Normalize()
	if !IsDirTarget(path) {
		b, err := Marshal(doc)
		if err != nil {
			return nil, err
		}
		if dir := filepath.Dir(path); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return nil, err
		}
		return &SaveResult{Written: []string{path}}, nil
	}
	return saveDir(doc, strings.TrimRight(path, "/"))
}

func saveDir(doc *Document, root string) (*SaveResult, error) {
	res := &SaveResult{Directory: true}
	hostRoot := filepath.Join(root, hostsDir)
	if err := os.MkdirAll(hostRoot, 0o755); err != nil {
		return nil, err
	}

	m := manifest{APIVersion: doc.APIVersion, Infrastructure: doc.Infrastructure}
	if m.APIVersion == "" {
		m.APIVersion = APIVersion
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(root, ManifestName)
	if err := os.WriteFile(manifestPath, b, 0o644); err != nil {
		return nil, err
	}
	res.Written = append(res.Written, manifestPath)

	if err := saveTemplates(doc, root, res); err != nil {
		return nil, err
	}

	kept := map[string]bool{}
	names := hostFileNames(doc.Hosts)
	for i := range doc.Hosts {
		name := names[i]
		p := filepath.Join(hostRoot, name)
		hb, err := yaml.Marshal(doc.Hosts[i])
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, hb, 0o644); err != nil {
			return nil, err
		}
		res.Written = append(res.Written, p)
		kept[name] = true
	}

	// Prune host files this export did not produce. An export is a snapshot;
	// a leftover file would read as a resource to create on the next apply.
	entries, err := os.ReadDir(hostRoot)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) || kept[e.Name()] {
			continue
		}
		p := filepath.Join(hostRoot, e.Name())
		if err := os.Remove(p); err != nil {
			return nil, err
		}
		res.Removed = append(res.Removed, p)
	}
	sort.Strings(res.Removed)
	return res, nil
}

// saveTemplates writes one file per template, pruning files this export did
// not produce. Pruning a template file only stops managing it here; diff never
// deletes a template on the platform.
func saveTemplates(doc *Document, root string, res *SaveResult) error {
	tplRoot := filepath.Join(root, templatesDir)
	if len(doc.Templates) == 0 {
		if _, err := os.Stat(tplRoot); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
	}
	if err := os.MkdirAll(tplRoot, 0o755); err != nil {
		return err
	}

	kept := map[string]bool{}
	names := templateFileNames(doc.Templates)
	for i := range doc.Templates {
		p := filepath.Join(tplRoot, names[i])
		b, err := yaml.Marshal(doc.Templates[i])
		if err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
		res.Written = append(res.Written, p)
		kept[names[i]] = true
	}

	entries, err := os.ReadDir(tplRoot)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) || kept[e.Name()] {
			continue
		}
		p := filepath.Join(tplRoot, e.Name())
		if err := os.Remove(p); err != nil {
			return err
		}
		res.Removed = append(res.Removed, p)
	}
	return nil
}

func templateFileNames(templates []Template) []string {
	out := make([]string, len(templates))
	used := map[string]int{}
	for i, t := range templates {
		base := slug(t.Name)
		if base == "" {
			base = "template"
		}
		name := base + ".yaml"
		if n, clash := used[base]; clash {
			if t.ID != "" {
				name = base + "-" + slug(t.ID) + ".yaml"
			} else {
				name = fmt.Sprintf("%s-%d.yaml", base, n+1)
			}
			used[base] = n + 1
		} else {
			used[base] = 1
		}
		out[i] = name
	}
	return out
}

// hostFileNames assigns a stable, unique filename to every host. Names are
// slugified, and collisions are broken deterministically so that repeated
// exports of unchanged state produce identical filenames.
func hostFileNames(hosts []Host) []string {
	out := make([]string, len(hosts))
	used := map[string]int{}
	for i, h := range hosts {
		base := slug(h.Name)
		if base == "" {
			base = "host"
		}
		name := base + ".yaml"
		if n, clash := used[base]; clash {
			// Prefer the id as the discriminator; fall back to a counter.
			if h.ID != "" {
				name = base + "-" + slug(h.ID) + ".yaml"
			} else {
				name = fmt.Sprintf("%s-%d.yaml", base, n+1)
			}
			used[base] = n + 1
		} else {
			used[base] = 1
		}
		out[i] = name
	}
	return out
}

func slug(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func isYAML(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

// Load reads a document from a file or an export directory.
func Load(path string) (*Document, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return loadDir(path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc Document
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	doc.Normalize()
	return &doc, nil
}

func loadTemplates(root string) ([]Template, error) {
	dir := filepath.Join(root, templatesDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && isYAML(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var out []Template
	for _, n := range names {
		p := filepath.Join(dir, n)
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var t Template
		if err := yaml.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("cannot parse %s: %w", p, err)
		}
		if strings.TrimSpace(t.Name) == "" {
			return nil, fmt.Errorf("%s has no name: every template file must set one", p)
		}
		out = append(out, t)
	}
	return out, nil
}

func loadDir(root string) (*Document, error) {
	mb, err := os.ReadFile(filepath.Join(root, ManifestName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s is a directory but has no %s; it is not an export directory",
			root, ManifestName)
	}
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := yaml.Unmarshal(mb, &m); err != nil {
		return nil, fmt.Errorf("cannot parse %s: %w", filepath.Join(root, ManifestName), err)
	}

	doc := &Document{APIVersion: m.APIVersion, Infrastructure: m.Infrastructure}

	tpls, err := loadTemplates(root)
	if err != nil {
		return nil, err
	}
	doc.Templates = tpls

	hostRoot := filepath.Join(root, hostsDir)
	entries, err := os.ReadDir(hostRoot)
	if errors.Is(err, fs.ErrNotExist) {
		doc.Normalize()
		return doc, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && isYAML(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, n := range names {
		p := filepath.Join(hostRoot, n)
		hb, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var h Host
		if err := yaml.Unmarshal(hb, &h); err != nil {
			return nil, fmt.Errorf("cannot parse %s: %w", p, err)
		}
		if strings.TrimSpace(h.Name) == "" {
			return nil, fmt.Errorf("%s has no name: every host file must set one", p)
		}
		doc.Hosts = append(doc.Hosts, h)
	}
	doc.Normalize()
	return doc, nil
}
