package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/skill"
)

// TestSkillDescribesOnlyRealCommands is requirement X10.
//
// A skill that names commands the binary does not have makes an agent
// confidently wrong, which is worse than shipping no skill at all. Review
// cannot be relied on to catch that, so it is enforced here: every `ups ...`
// invocation in SKILL.md must resolve to a real command.
func TestSkillDescribesOnlyRealCommands(t *testing.T) {
	app := &App{Stdin: nil, Stdout: os.Stdout, Stderr: os.Stderr}
	root := NewRoot(app)

	referenced := skill.ReferencedCommands(skill.Content)
	if len(referenced) == 0 {
		t.Fatal("no commands found in the skill; the extractor is broken")
	}

	for _, path := range referenced {
		args := strings.Fields(path)
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Errorf("SKILL.md references `ups %s`, which is not a command: %v", path, err)
			continue
		}
		// Find falls back to the closest parent, so confirm the full path
		// actually resolved rather than partially matching.
		if !resolvesFully(cmd, args) {
			t.Errorf("SKILL.md references `ups %s`, but only `ups %s` exists",
				path, commandPath(cmd))
		}
	}
}

// resolvesFully reports whether every word in args was consumed by the command
// tree rather than left over as an argument.
func resolvesFully(cmd *cobra.Command, args []string) bool {
	resolved := strings.Fields(commandPath(cmd))
	if len(resolved) > len(args) {
		return false
	}
	// Every resolved word must match the head of args in order.
	for i, w := range resolved {
		if args[i] != w {
			return false
		}
	}
	// Any remaining words must be positional arguments, not missing
	// subcommands: if the command has children, an unmatched word is a typo.
	if len(args) > len(resolved) && cmd.HasSubCommands() {
		next := args[len(resolved)]
		for _, sub := range cmd.Commands() {
			if sub.Name() == next {
				return false // should have resolved deeper
			}
		}
		return false
	}
	return true
}

// commandPath returns the subcommand path without the root name.
func commandPath(cmd *cobra.Command) string {
	full := cmd.CommandPath()
	return strings.TrimSpace(strings.TrimPrefix(full, "ups"))
}

// The skill must keep explaining why, not just how. If these disappear, the
// document has been reduced to a command list and lost its purpose.
func TestSkillExplainsTheReasoning(t *testing.T) {
	required := []struct{ topic, phrase string }{
		{"silent monitoring loss", "nothing pages anyone"},
		{"diff before apply", "safe to run unread"},
		{"runbook preflight", "partially configured"},
		{"secrets not in argv", "shell history"},
		{"cross-customer ambiguity", "wrong customer's device"},
		{"doctor vs healthcheck", "ups infra healthcheck"},
		{"client-side log filtering", "no query"},
	}
	for _, r := range required {
		if !strings.Contains(skill.Content, r.phrase) {
			t.Errorf("the skill no longer explains %s (expected to find %q)", r.topic, r.phrase)
		}
	}
}

func TestSkillHasValidFrontmatter(t *testing.T) {
	if !strings.HasPrefix(skill.Content, "---\n") {
		t.Fatal("the skill must start with YAML frontmatter")
	}
	end := strings.Index(skill.Content[4:], "\n---\n")
	if end < 0 {
		t.Fatal("frontmatter is not terminated")
	}
	fm := skill.Content[4 : end+4]
	for _, key := range []string{"name:", "description:"} {
		if !strings.Contains(fm, key) {
			t.Errorf("frontmatter is missing %q", key)
		}
	}
	if !strings.Contains(fm, "name: "+skill.Name) {
		t.Errorf("frontmatter name must be %q", skill.Name)
	}
}
