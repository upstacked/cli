// Package skill embeds, installs and verifies the Upstacked agent skill.
//
// A skill that describes a different command surface than the installed binary
// is worse than no skill: it makes an agent confidently wrong. So installation
// records a manifest, and `ups doctor` can tell an outdated skill apart from
// one the user deliberately edited (story J6).
package skill

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/upstacked/cli/internal/errs"
)

//go:embed SKILL.md
var Content string

// Name is the skill directory name and the agent-facing skill name.
const Name = "upstacked"

// Scope decides where the skill is installed.
type Scope string

const (
	// ScopeUser installs to ~/.claude/skills, available in every project.
	ScopeUser Scope = "user"
	// ScopeProject installs to ./.claude/skills, committed with the repo.
	ScopeProject Scope = "project"
)

// Manifest records what was installed, so drift is detectable.
type Manifest struct {
	Version  string `json:"version"`
	Checksum string `json:"checksum"`
}

const manifestName = ".ups-skill.json"

// Checksum is the content hash used to detect local edits.
func Checksum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// EmbeddedChecksum is the hash of the skill this binary ships.
func EmbeddedChecksum() string { return Checksum(Content) }

// Dir returns the install directory for a scope.
func Dir(scope Scope, projectRoot string) (string, error) {
	switch scope {
	case ScopeProject:
		if projectRoot == "" {
			wd, err := os.Getwd()
			if err != nil {
				return "", errs.General("cannot determine working directory").Wrapping(err)
			}
			projectRoot = wd
		}
		return filepath.Join(projectRoot, ".claude", "skills", Name), nil
	case ScopeUser, "":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errs.General("cannot determine home directory").Wrapping(err)
		}
		return filepath.Join(home, ".claude", "skills", Name), nil
	default:
		return "", errs.Usage("unknown scope %q", scope).WithHint("use --scope user or --scope project")
	}
}

// State describes an installation.
type State struct {
	Dir       string
	Path      string
	Installed bool
	// Modified means the on-disk content differs from what was installed.
	Modified bool
	// Outdated means it was installed by a different CLI version.
	Outdated bool
	// Current means installed, unmodified, and matching this binary.
	Current         bool
	InstalledVer    string
	OnDiskChecksum  string
	ExpectedChecksm string
}

// Inspect reports the installation state without changing anything.
func Inspect(scope Scope, projectRoot, version string) (*State, error) {
	dir, err := Dir(scope, projectRoot)
	if err != nil {
		return nil, err
	}
	st := &State{Dir: dir, Path: filepath.Join(dir, "SKILL.md"), ExpectedChecksm: EmbeddedChecksum()}

	b, err := os.ReadFile(st.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, errs.General("cannot read %s", st.Path).Wrapping(err)
	}
	st.Installed = true
	st.OnDiskChecksum = Checksum(string(b))

	m, merr := readManifest(dir)
	if merr == nil && m != nil {
		st.InstalledVer = m.Version
		// Modified means: differs from what *this* install wrote.
		st.Modified = m.Checksum != st.OnDiskChecksum
		st.Outdated = m.Checksum != st.ExpectedChecksm && !st.Modified
	} else {
		// No manifest: cannot distinguish edited from foreign. Compare directly.
		st.Modified = st.OnDiskChecksum != st.ExpectedChecksm
	}
	st.Current = st.Installed && !st.Modified && st.OnDiskChecksum == st.ExpectedChecksm
	return st, nil
}

// Install writes the skill. It refuses to overwrite local edits unless forced,
// because silently discarding a user's customisation is worse than a failure.
func Install(scope Scope, projectRoot, version string, force bool) (*State, error) {
	st, err := Inspect(scope, projectRoot, version)
	if err != nil {
		return nil, err
	}
	if st.Installed && st.Modified && !force {
		return st, errs.Conflict("the installed skill at %s has local edits", st.Path).
			WithHint("keep them, or overwrite with: ups skill install --force")
	}
	if st.Current && !force {
		return st, nil
	}
	if err := os.MkdirAll(st.Dir, 0o755); err != nil {
		return nil, errs.General("cannot create %s", st.Dir).Wrapping(err)
	}
	if err := os.WriteFile(st.Path, []byte(Content), 0o644); err != nil {
		return nil, errs.General("cannot write %s", st.Path).Wrapping(err)
	}
	if err := writeManifest(st.Dir, &Manifest{Version: version, Checksum: EmbeddedChecksum()}); err != nil {
		return nil, err
	}
	return Inspect(scope, projectRoot, version)
}

// Uninstall removes an installed skill.
func Uninstall(scope Scope, projectRoot string) (string, error) {
	dir, err := Dir(scope, projectRoot)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return dir, errs.NotFound("no skill installed at %s", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return dir, errs.General("cannot remove %s", dir).Wrapping(err)
	}
	return dir, nil
}

func readManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func writeManifest(dir string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return errs.General("cannot serialize skill manifest").Wrapping(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName), append(b, '\n'), 0o644); err != nil {
		return errs.General("cannot write skill manifest").Wrapping(err)
	}
	return nil
}

// Invocation is one `ups ...` example found in the skill.
type Invocation struct {
	// Path is the subcommand path, e.g. "monitoring item test".
	Path string
	// Flags are the long flags used, without the leading dashes.
	Flags []string
	// Line is the source text, for error messages.
	Line string
}

// ReferencedCommands extracts every `ups ...` command path mentioned in the
// skill. The drift test (requirement X10) uses this to prove the skill only
// describes commands that actually exist.
func ReferencedCommands(content string) []string {
	var out []string
	seen := map[string]bool{}
	for _, inv := range ReferencedInvocations(content) {
		if !seen[inv.Path] {
			seen[inv.Path] = true
			out = append(out, inv.Path)
		}
	}
	return out
}

// ReferencedInvocations extracts command paths together with their flags.
//
// Checking only command names is not enough: a skill can name a real command
// and still describe a flag that does not exist, or one whose meaning has
// changed. That is drift too, and it misleads an agent just as badly.
func ReferencedInvocations(content string) []Invocation {
	var out []Invocation
	for _, line := range strings.Split(content, "\n") {
		if inv, ok := parseInvocation(line); ok {
			out = append(out, inv)
		}
	}
	return out
}

// parseInvocation pulls the command path and long flags out of one line.
func parseInvocation(line string) (Invocation, bool) {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "| ")
	trimmed = strings.TrimPrefix(trimmed, "`")
	if !strings.HasPrefix(trimmed, "ups ") {
		return Invocation{}, false
	}
	fields := strings.Fields(trimmed)

	var path []string
	var flags []string
	pathDone := false
	for _, f := range fields[1:] {
		f = strings.Trim(f, "`|,.")
		if f == "" {
			continue
		}
		// A shell pipe or redirect ends the invocation.
		if f == "|" || f == ">" || f == "&&" || strings.HasPrefix(f, "$") {
			break
		}
		if strings.HasPrefix(f, "--") {
			pathDone = true
			name := strings.TrimPrefix(f, "--")
			if i := strings.Index(name, "="); i >= 0 {
				name = name[:i]
			}
			if name != "" && isFlagWord(name) {
				flags = append(flags, name)
			}
			continue
		}
		if pathDone || strings.HasPrefix(f, "-") || strings.HasPrefix(f, "<") ||
			strings.ContainsAny(f, "|/\\\"'") || !isCommandWord(f) {
			// Positional arguments and placeholders are not subcommands, but
			// flags may still follow them.
			pathDone = true
			continue
		}
		path = append(path, f)
	}
	if len(path) == 0 {
		return Invocation{}, false
	}
	return Invocation{Path: strings.Join(path, " "), Flags: flags, Line: trimmed}, true
}

func isFlagWord(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && r != '-' {
			return false
		}
	}
	return true
}

func isCommandWord(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && r != '-' {
			return false
		}
	}
	return true
}
