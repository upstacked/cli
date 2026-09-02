package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

// projectEnv runs commands with the working directory inside a temp project,
// so project-scope installs cannot write into the real repository.
func projectEnv(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	root := t.TempDir()
	t.Chdir(root)
	return e, root
}

func TestSkillInstallsIntoSelectedClients(t *testing.T) {
	e, root := projectEnv(t)

	res := e.run("skill", "install", "--client", "claude,agents,cursor", "--scope", "project")
	if res.ExitCode != 0 {
		t.Fatalf("install failed: %s", res.Stderr)
	}
	for _, p := range []string{
		filepath.Join(".claude", "skills", "upstacked", "SKILL.md"),
		"AGENTS.md",
		filepath.Join(".cursor", "rules", "upstacked.mdc"),
	} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Errorf("expected %s to be written: %v", p, err)
		}
	}
	// A client that was not selected must be left alone.
	if _, err := os.Stat(filepath.Join(root, ".github", "copilot-instructions.md")); !os.IsNotExist(err) {
		t.Error("an unselected client should not be written")
	}
}

// The headline safety property: installing must not destroy instructions the
// user already wrote.
func TestSkillInstallPreservesExistingAgentsFile(t *testing.T) {
	e, root := projectEnv(t)
	agents := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# House rules\n\nAlways run tests.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if res := e.run("skill", "install", "--client", "agents", "--scope", "project"); res.ExitCode != 0 {
		t.Fatalf("install failed: %s", res.Stderr)
	}
	b, err := os.ReadFile(agents)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "Always run tests.") {
		t.Fatal("the user's own AGENTS.md content was destroyed")
	}
	if !strings.Contains(got, "Upstacked CLI") {
		t.Error("the skill content should have been appended")
	}

	// And removing it must put the file back the way it was.
	if res := e.run("skill", "uninstall", "--client", "agents", "--scope", "project", "--yes"); res.ExitCode != 0 {
		t.Fatalf("uninstall failed: %s", res.Stderr)
	}
	b, _ = os.ReadFile(agents)
	if !strings.Contains(string(b), "Always run tests.") {
		t.Error("uninstall destroyed the user's content")
	}
	if strings.Contains(string(b), "Upstacked CLI") {
		t.Error("the managed block should be gone")
	}
}

// Without a terminal the picker is impossible, so it must fail with the flag
// to use rather than hang.
func TestSkillInstallNeedsClientWhenNotATerminal(t *testing.T) {
	e, _ := projectEnv(t)
	res := e.run("skill", "install", "--scope", "project")
	if res.ExitCode != errs.CodeUsage {
		t.Errorf("expected usage exit %d, got %d: %s", errs.CodeUsage, res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "--client")
}

func TestSkillRejectsUnknownClient(t *testing.T) {
	e, _ := projectEnv(t)
	res := e.run("skill", "install", "--client", "emacs-doctor", "--scope", "project")
	if res.ExitCode != errs.CodeUsage {
		t.Errorf("expected usage exit, got %d", res.ExitCode)
	}
	contains(t, res.Stderr, "unknown client")
}

// A client with no location at the chosen scope must say so, not be dropped.
func TestSkillRejectsClientWithoutThatScope(t *testing.T) {
	e, _ := projectEnv(t)
	res := e.run("skill", "install", "--client", "cursor", "--scope", "user")
	if res.ExitCode != errs.CodeUsage {
		t.Errorf("expected usage exit, got %d: %s", res.ExitCode, res.Stderr)
	}
	contains(t, res.Stderr, "--scope project")
}

func TestSkillStatusAndClients(t *testing.T) {
	e, _ := projectEnv(t)
	if res := e.run("skill", "install", "--client", "agents", "--scope", "project"); res.ExitCode != 0 {
		t.Fatalf("install failed: %s", res.Stderr)
	}

	res := e.run("skill", "status", "--scope", "project")
	if res.ExitCode != 0 {
		t.Fatalf("status failed: %s", res.Stderr)
	}
	contains(t, res.Stdout, "AGENTS.md")
	contains(t, res.Stdout, "up to date")

	res = e.run("skill", "clients")
	if res.ExitCode != 0 {
		t.Fatalf("clients failed: %s", res.Stderr)
	}
	for _, want := range []string{"claude", "cursor", "copilot", "gemini"} {
		contains(t, res.Stdout, want)
	}
}

// doctor must notice a stale copy in any client, not just the first.
func TestDoctorReportsEveryInstalledClient(t *testing.T) {
	e, root := projectEnv(t)
	if res := e.run("skill", "install", "--client", "agents,cursor", "--scope", "project"); res.ExitCode != 0 {
		t.Fatalf("install failed: %s", res.Stderr)
	}
	// Make one of them look like it came from an older build.
	rule := filepath.Join(root, ".cursor", "rules", "upstacked.mdc")
	b, err := os.ReadFile(rule)
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.Replace(string(b), "sha=", "sha=0000", 1)
	if err := os.WriteFile(rule, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run("doctor")
	contains(t, res.Stdout, "skill: agents")
	contains(t, res.Stdout, "skill: cursor")
}

func TestSkillShowBody(t *testing.T) {
	e, _ := projectEnv(t)
	res := e.run("skill", "show", "--body")
	if res.ExitCode != 0 {
		t.Fatalf("show failed: %s", res.Stderr)
	}
	if strings.HasPrefix(res.Stdout, "---") {
		t.Error("--body should omit the Claude Code frontmatter")
	}
	contains(t, res.Stdout, "# Upstacked CLI")
}
