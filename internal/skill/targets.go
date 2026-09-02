package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scope decides whether an install is per-user or per-project.
type Scope string

const (
	// ScopeUser installs where the tool looks for personal instructions.
	ScopeUser Scope = "user"
	// ScopeProject installs into the repository, to be committed.
	ScopeProject Scope = "project"
)

// Mode describes who owns the destination file.
type Mode int

const (
	// ModeOwnFile means the file belongs entirely to this skill and can be
	// written wholesale.
	ModeOwnFile Mode = iota
	// ModeSharedBlock means the file belongs to the user and may already hold
	// their own instructions. Only a delimited block is managed; everything
	// else is preserved. Overwriting these files would silently destroy
	// someone's carefully written guidance.
	ModeSharedBlock
)

// Target is one LLM client's convention for agent instructions.
type Target struct {
	ID     string
	Name   string
	Mode   Mode
	Scopes []Scope
	// Note explains coverage or caveats in the picker.
	Note string
	// path returns the destination for a scope, relative to root for project
	// scope. An empty string means the scope is unsupported.
	path func(scope Scope, root string) (string, error)
	// render turns the shared skill body into this client's format.
	render func(body string) string
}

// Supports reports whether the target can be installed at a scope.
func (t Target) Supports(scope Scope) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Path resolves the destination file for a scope.
func (t Target) Path(scope Scope, root string) (string, error) {
	if !t.Supports(scope) {
		return "", fmt.Errorf("%s has no %s-scope location", t.Name, scope)
	}
	return t.path(scope, root)
}

// Where describes the destination for help and picker text.
func (t Target) Where(scope Scope) string {
	p, err := t.Path(scope, ".")
	if err != nil {
		return "not supported at " + string(scope) + " scope"
	}
	if scope == ScopeUser {
		return collapseHome(p)
	}
	return strings.TrimPrefix(filepath.ToSlash(p), "./")
}

func collapseHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func userPath(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, parts...)...), nil
}

func projectPath(root string, parts ...string) (string, error) {
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root = wd
	}
	return filepath.Join(append([]string{root}, parts...)...), nil
}

// Targets is the registry, in the order the picker shows them.
//
// Only conventions the tools actually document are listed. Inventing a path
// would put guidance somewhere nothing reads it, which is worse than not
// installing at all.
var Targets = []Target{
	{
		ID: "claude", Name: "Claude Code", Mode: ModeOwnFile,
		Scopes: []Scope{ScopeUser, ScopeProject},
		Note:   "a real skill, loaded on demand",
		path: func(s Scope, root string) (string, error) {
			if s == ScopeUser {
				return userPath(".claude", "skills", Name, "SKILL.md")
			}
			return projectPath(root, ".claude", "skills", Name, "SKILL.md")
		},
		render: func(body string) string { return Content },
	},
	{
		ID: "agents", Name: "AGENTS.md", Mode: ModeSharedBlock,
		Scopes: []Scope{ScopeProject},
		Note:   "Codex, Amp, Zed, opencode, Jules and others",
		path: func(s Scope, root string) (string, error) {
			return projectPath(root, "AGENTS.md")
		},
		render: plainMarkdown,
	},
	{
		ID: "codex", Name: "OpenAI Codex (global)", Mode: ModeSharedBlock,
		Scopes: []Scope{ScopeUser},
		Note:   "per-project Codex is covered by AGENTS.md",
		path: func(s Scope, root string) (string, error) {
			return userPath(".codex", "AGENTS.md")
		},
		render: plainMarkdown,
	},
	{
		ID: "cursor", Name: "Cursor", Mode: ModeOwnFile,
		Scopes: []Scope{ScopeProject},
		path: func(s Scope, root string) (string, error) {
			return projectPath(root, ".cursor", "rules", Name+".mdc")
		},
		render: cursorRule,
	},
	{
		ID: "copilot", Name: "GitHub Copilot", Mode: ModeSharedBlock,
		Scopes: []Scope{ScopeProject},
		path: func(s Scope, root string) (string, error) {
			return projectPath(root, ".github", "copilot-instructions.md")
		},
		render: plainMarkdown,
	},
	{
		ID: "windsurf", Name: "Windsurf", Mode: ModeOwnFile,
		Scopes: []Scope{ScopeProject},
		path: func(s Scope, root string) (string, error) {
			return projectPath(root, ".windsurf", "rules", Name+".md")
		},
		render: plainMarkdown,
	},
	{
		ID: "cline", Name: "Cline", Mode: ModeOwnFile,
		Scopes: []Scope{ScopeProject},
		path: func(s Scope, root string) (string, error) {
			return projectPath(root, ".clinerules", Name+".md")
		},
		render: plainMarkdown,
	},
	{
		ID: "roo", Name: "Roo Code", Mode: ModeOwnFile,
		Scopes: []Scope{ScopeProject},
		path: func(s Scope, root string) (string, error) {
			return projectPath(root, ".roo", "rules", Name+".md")
		},
		render: plainMarkdown,
	},
	{
		ID: "continue", Name: "Continue", Mode: ModeOwnFile,
		Scopes: []Scope{ScopeUser, ScopeProject},
		path: func(s Scope, root string) (string, error) {
			if s == ScopeUser {
				return userPath(".continue", "rules", Name+".md")
			}
			return projectPath(root, ".continue", "rules", Name+".md")
		},
		render: plainMarkdown,
	},
	{
		ID: "gemini", Name: "Gemini CLI", Mode: ModeSharedBlock,
		Scopes: []Scope{ScopeUser, ScopeProject},
		path: func(s Scope, root string) (string, error) {
			if s == ScopeUser {
				return userPath(".gemini", "GEMINI.md")
			}
			return projectPath(root, "GEMINI.md")
		},
		render: plainMarkdown,
	},
	{
		ID: "junie", Name: "JetBrains Junie", Mode: ModeSharedBlock,
		Scopes: []Scope{ScopeProject},
		path: func(s Scope, root string) (string, error) {
			return projectPath(root, ".junie", "guidelines.md")
		},
		render: plainMarkdown,
	},
}

// DefaultTargets are pre-selected in the picker: a real skill for Claude Code,
// and AGENTS.md, which several tools read.
var DefaultTargets = []string{"claude", "agents"}

// TargetByID looks up one target.
func TargetByID(id string) (Target, bool) {
	for _, t := range Targets {
		if t.ID == strings.ToLower(strings.TrimSpace(id)) {
			return t, true
		}
	}
	return Target{}, false
}

// TargetIDs lists every known client id.
func TargetIDs() []string {
	out := make([]string, 0, len(Targets))
	for _, t := range Targets {
		out = append(out, t.ID)
	}
	sort.Strings(out)
	return out
}

// ResolveTargets turns user-supplied ids into targets, rejecting unknown ones
// rather than silently installing a subset.
func ResolveTargets(ids []string, scope Scope) ([]Target, error) {
	var out []Target
	seen := map[string]bool{}
	for _, raw := range ids {
		for _, id := range strings.Split(raw, ",") {
			id = strings.ToLower(strings.TrimSpace(id))
			if id == "" {
				continue
			}
			if id == "all" {
				for _, t := range Targets {
					if t.Supports(scope) && !seen[t.ID] {
						seen[t.ID] = true
						out = append(out, t)
					}
				}
				continue
			}
			t, ok := TargetByID(id)
			if !ok {
				return nil, fmt.Errorf("unknown client %q (known: %s)",
					id, strings.Join(TargetIDs(), ", "))
			}
			if !t.Supports(scope) {
				return nil, fmt.Errorf("%s has no %s-scope location; try --scope %s",
					t.Name, scope, otherScope(scope))
			}
			if !seen[t.ID] {
				seen[t.ID] = true
				out = append(out, t)
			}
		}
	}
	return out, nil
}

func otherScope(s Scope) Scope {
	if s == ScopeUser {
		return ScopeProject
	}
	return ScopeUser
}

// plainMarkdown strips the YAML frontmatter, which only Claude Code consumes,
// and gives the document a heading so it reads correctly when concatenated
// into a larger instructions file.
func plainMarkdown(body string) string { return body }

// cursorRule wraps the body in the frontmatter Cursor expects for a rule.
func cursorRule(body string) string {
	return "---\ndescription: " + oneLineDescription() + "\nalwaysApply: false\n---\n\n" + body
}
