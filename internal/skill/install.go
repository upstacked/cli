package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Managed content carries a marker recording the version and a checksum of the
// body as written. That makes every install self-describing: no sidecar state
// file, and "outdated" can be told apart from "the user edited this".
const (
	beginMarker = "<!-- BEGIN ups:skill"
	endMarker   = "<!-- END ups:skill -->"
)

var markerRe = regexp.MustCompile(`<!-- BEGIN ups:skill version=(\S+) sha=([0-9a-f]+) -->`)

func markerLine(version, sha string) string {
	return fmt.Sprintf("%s version=%s sha=%s -->", beginMarker, version, sha)
}

// wrap renders content with its markers.
//
// The begin marker goes *after* any YAML frontmatter. Claude Code and Cursor
// only parse frontmatter when it is the very first thing in the file, so a
// marker above it silently turns the skill's name and description into
// garbage — the skill still installs, and is then useless.
func wrap(content, version string) string {
	sha := Checksum(content)
	front, rest := splitFrontmatter(content)
	return front + markerLine(version, sha) + "\n" + strings.TrimRight(rest, "\n") + "\n" + endMarker + "\n"
}

// splitFrontmatter separates a leading YAML block from the rest, keeping the
// trailing newline on the frontmatter so it stays flush with the top.
func splitFrontmatter(content string) (front, rest string) {
	if !strings.HasPrefix(content, "---\n") {
		return "", content
	}
	i := strings.Index(content[4:], "\n---\n")
	if i < 0 {
		return "", content
	}
	end := 4 + i + len("\n---\n")
	return content[:end], strings.TrimLeft(content[end:], "\n")
}

// ownedContent returns the part of a file this skill is responsible for, which
// is the whole thing for a file it owns and just the block for a shared one.
func ownedContent(t Target, raw, block string) string {
	if t.Mode != ModeOwnFile {
		return block
	}
	out := markerRe.ReplaceAllString(raw, "")
	out = strings.ReplaceAll(out, endMarker, "")
	return out
}

// State is the installation status of one target.
type State struct {
	Target    Target
	Scope     Scope
	Path      string
	Installed bool
	// Modified means the managed content on disk no longer matches what was
	// written, so someone edited it by hand.
	Modified bool
	// Outdated means it was written from a different version of the skill.
	Outdated bool
	Current  bool
	Version  string
}

// Summary renders a one-line status for humans.
func (s State) Summary() string {
	switch {
	case !s.Installed:
		return "not installed"
	case s.Modified:
		return "locally edited"
	case s.Outdated:
		return "outdated (" + s.Version + ")"
	default:
		return "up to date"
	}
}

// Inspect reports a target's state without changing anything.
func Inspect(t Target, scope Scope, root, version string) (*State, error) {
	path, err := t.Path(scope, root)
	if err != nil {
		return nil, err
	}
	st := &State{Target: t, Scope: scope, Path: path}

	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}

	block, declaredVersion, declaredSha, found := extractBlock(string(raw))
	if !found {
		return st, nil
	}
	st.Installed = true
	st.Version = declaredVersion

	// The checksum covers everything this skill owns: the whole file for a
	// file it owns (frontmatter included, so an edit above the marker counts),
	// and only the block for a file shared with the user's own instructions.
	owned := ownedContent(t, string(raw), block)
	st.Modified = Checksum(owned) != declaredSha
	st.Outdated = !st.Modified && declaredSha != Checksum(t.render(Body()))
	st.Current = st.Installed && !st.Modified && !st.Outdated
	return st, nil
}

// extractBlock pulls the managed content out of a file, with the version and
// checksum recorded when it was written.
func extractBlock(content string) (block, version, sha string, ok bool) {
	m := markerRe.FindStringSubmatchIndex(content)
	if m == nil {
		return "", "", "", false
	}
	groups := markerRe.FindStringSubmatch(content)
	version, sha = groups[1], groups[2]

	afterBegin := m[1]
	end := strings.Index(content[afterBegin:], endMarker)
	if end < 0 {
		return "", "", "", false
	}
	block = strings.TrimSpace(content[afterBegin : afterBegin+end])
	return block, version, sha, true
}

// Install writes or updates a target.
//
// For a shared file only the managed block is touched; anything the user wrote
// around it is preserved, because that file is theirs and may hold guidance
// nothing else records.
func Install(t Target, scope Scope, root, version string, force bool) (*State, error) {
	st, err := Inspect(t, scope, root, version)
	if err != nil {
		return nil, err
	}
	if st.Installed && st.Modified && !force {
		return st, fmt.Errorf("%s has local edits at %s", t.Name, st.Path)
	}
	if st.Current && !force {
		return st, nil
	}

	content := t.render(Body())
	managed := wrap(content, version)

	if err := os.MkdirAll(filepath.Dir(st.Path), 0o755); err != nil {
		return nil, err
	}

	existing, err := os.ReadFile(st.Path)
	if errors.Is(err, fs.ErrNotExist) {
		existing = nil
	} else if err != nil {
		return nil, err
	}

	var out string
	switch {
	case t.Mode == ModeOwnFile:
		out = managed
	case len(existing) == 0:
		out = managed
	default:
		out = replaceOrAppend(string(existing), managed)
	}

	if err := os.WriteFile(st.Path, []byte(out), 0o644); err != nil {
		return nil, err
	}
	return Inspect(t, scope, root, version)
}

// replaceOrAppend swaps an existing managed block, or appends one, leaving
// every other line of the user's file untouched.
func replaceOrAppend(existing, managed string) string {
	m := markerRe.FindStringIndex(existing)
	if m == nil {
		trimmed := strings.TrimRight(existing, "\n")
		if trimmed == "" {
			return managed
		}
		return trimmed + "\n\n" + managed
	}
	end := strings.Index(existing[m[1]:], endMarker)
	if end < 0 {
		// A begin without an end: treat everything from the marker on as ours
		// rather than duplicating the block.
		return strings.TrimRight(existing[:m[0]], "\n") + "\n\n" + managed
	}
	tail := existing[m[1]+end+len(endMarker):]
	head := existing[:m[0]]
	return strings.TrimRight(head, "\n") + "\n" + managed + strings.TrimLeft(tail, "\n")
}

// Uninstall removes the managed content, leaving the user's own text in place.
func Uninstall(t Target, scope Scope, root string) (string, error) {
	path, err := t.Path(scope, root)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, fmt.Errorf("nothing installed at %s", path)
	}
	if err != nil {
		return path, err
	}
	if _, _, _, ok := extractBlock(string(raw)); !ok {
		return path, fmt.Errorf("no managed block at %s", path)
	}

	if t.Mode == ModeOwnFile {
		if err := os.Remove(path); err != nil {
			return path, err
		}
		// Tidy the directory only if this skill created it and it is now empty.
		_ = os.Remove(filepath.Dir(path))
		return path, nil
	}

	remaining := stripBlock(string(raw))
	if strings.TrimSpace(remaining) == "" {
		// The file held nothing but our block, so it was ours after all.
		if err := os.Remove(path); err != nil {
			return path, err
		}
		return path, nil
	}
	return path, os.WriteFile(path, []byte(remaining), 0o644)
}

func stripBlock(content string) string {
	m := markerRe.FindStringIndex(content)
	if m == nil {
		return content
	}
	end := strings.Index(content[m[1]:], endMarker)
	if end < 0 {
		return strings.TrimRight(content[:m[0]], "\n") + "\n"
	}
	head := strings.TrimRight(content[:m[0]], "\n")
	tail := strings.TrimLeft(content[m[1]+end+len(endMarker):], "\n")
	switch {
	case head == "":
		return tail
	case tail == "":
		return head + "\n"
	default:
		return head + "\n\n" + tail
	}
}

// InstalledStates reports every target that has something on disk, across both
// scopes, so doctor can notice a stale install the user forgot about.
//
// Results are deduplicated by resolved path: when the working directory is the
// home directory the two scopes name the same file, and reporting it twice
// would suggest two installs that can drift apart.
func InstalledStates(root, version string) []*State {
	var out []*State
	seen := map[string]bool{}
	for _, t := range Targets {
		for _, scope := range []Scope{ScopeUser, ScopeProject} {
			if !t.Supports(scope) {
				continue
			}
			st, err := Inspect(t, scope, root, version)
			if err != nil || !st.Installed {
				continue
			}
			abs, err := filepath.Abs(st.Path)
			if err != nil {
				abs = st.Path
			}
			if seen[abs] {
				continue
			}
			seen[abs] = true
			out = append(out, st)
		}
	}
	return out
}
