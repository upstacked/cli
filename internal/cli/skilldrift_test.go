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

// TestSkillUsesOnlyRealFlags is the other half of X10.
//
// Checking command names alone is not enough. The skill once documented
// `ups export --out ./infra/` while --out only ever wrote a single file: a
// real command, a real flag, and wrong guidance. Flags are verified here so
// that kind of drift fails the build too.
func TestSkillUsesOnlyRealFlags(t *testing.T) {
	app := &App{Stdin: nil, Stdout: os.Stdout, Stderr: os.Stderr}
	root := NewRoot(app)

	for _, inv := range skill.ReferencedInvocations(skill.Content) {
		if len(inv.Flags) == 0 {
			continue
		}
		cmd, _, err := root.Find(strings.Fields(inv.Path))
		if err != nil {
			continue // reported by TestSkillDescribesOnlyRealCommands
		}
		for _, flag := range inv.Flags {
			if cmd.Flags().Lookup(flag) != nil || cmd.InheritedFlags().Lookup(flag) != nil {
				continue
			}
			if root.PersistentFlags().Lookup(flag) != nil {
				continue
			}
			t.Errorf("SKILL.md uses --%s on `ups %s`, which has no such flag\n  line: %s",
				flag, inv.Path, inv.Line)
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
		// Two log backends print the same table and mean different things.
		// An agent that cannot tell them apart will report a capped local
		// filter as a conclusive index search.
		{"log fallback is not an index search", "never fetched"},
		// An agent runs without a TTY, so the confirmation and login rules are
		// the difference between working and failing on the first mutation.
		{"no-TTY operation", "without a terminal"},
		{"--yes is not a safety override", "not the *safety*"},
		{"apply is not transactional", "Nothing is rolled back"},
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
