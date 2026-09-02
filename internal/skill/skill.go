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
	"strings"
)

//go:embed SKILL.md
var Content string

// Name is the skill directory name and the agent-facing skill name.
const Name = "upstacked"

// Body returns the skill without its YAML frontmatter.
//
// Only Claude Code consumes that frontmatter; every other client expects plain
// markdown, and a stray YAML block at the top of AGENTS.md is noise at best.
func Body() string {
	s := Content
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	if i := strings.Index(s[4:], "\n---\n"); i >= 0 {
		return strings.TrimLeft(s[4+i+len("\n---\n"):], "\n")
	}
	return s
}

// oneLineDescription returns the frontmatter description, for clients whose
// rule format wants a summary field.
func oneLineDescription() string {
	for _, line := range strings.Split(Content, "\n") {
		if strings.HasPrefix(line, "description:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "description:"))
		}
		if line == "---" && strings.Contains(Content, "description:") {
			continue
		}
	}
	return "Operate Upstacked infrastructure via the ups CLI"
}

// Checksum is the content hash used to detect local edits.
func Checksum(s string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(sum[:])[:16]
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
