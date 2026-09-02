package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func target(t *testing.T, id string) Target {
	t.Helper()
	tg, ok := TargetByID(id)
	if !ok {
		t.Fatalf("no such target %q", id)
	}
	return tg
}

func TestInstallOwnFileWritesWholeFile(t *testing.T) {
	root := t.TempDir()
	st, err := Install(target(t, "cursor"), ScopeProject, root, "1.2.3", false)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Current {
		t.Errorf("expected a current install, got %s", st.Summary())
	}
	b, err := os.ReadFile(filepath.Join(root, ".cursor", "rules", Name+".mdc"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	// Frontmatter must be the very first thing in the file; a marker above it
	// stops the client parsing it at all.
	if !strings.HasPrefix(content, "---\n") {
		t.Error("a Cursor rule must start with its frontmatter, not the marker")
	}
	if !strings.Contains(content, "alwaysApply:") {
		t.Error("a Cursor rule needs its own frontmatter")
	}
	if !strings.Contains(content, beginMarker) {
		t.Error("managed content should carry the marker")
	}
	if strings.Contains(content, "name: upstacked\n") {
		t.Error("Claude Code frontmatter must not leak into other clients")
	}
}

// The whole point of shared-file mode: a user's own instructions survive.
func TestInstallIntoSharedFilePreservesUserContent(t *testing.T) {
	root := t.TempDir()
	agents := filepath.Join(root, "AGENTS.md")
	original := "# Our conventions\n\nRun the linter before committing.\n"
	if err := os.WriteFile(agents, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(target(t, "agents"), ScopeProject, root, "1.2.3", false); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, agents)
	if !strings.Contains(got, "Run the linter before committing.") {
		t.Fatal("the user's existing instructions were destroyed")
	}
	if !strings.Contains(got, beginMarker) || !strings.Contains(got, endMarker) {
		t.Error("the managed block should have been appended")
	}
	if !strings.HasPrefix(got, "# Our conventions") {
		t.Error("the user's content should stay at the top")
	}
}

// Re-installing must replace the block, not stack up copies.
func TestReinstallReplacesBlockInPlace(t *testing.T) {
	root := t.TempDir()
	agents := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# Mine\n\nkeep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := Install(target(t, "agents"), ScopeProject, root, "1.2.3", true); err != nil {
			t.Fatal(err)
		}
	}
	got := readFile(t, agents)
	if n := strings.Count(got, beginMarker); n != 1 {
		t.Errorf("expected exactly one managed block, found %d", n)
	}
	if !strings.Contains(got, "keep me") {
		t.Error("user content lost across re-installs")
	}
}

// Content written after the block must survive an update too.
func TestUpdatePreservesContentOnBothSides(t *testing.T) {
	root := t.TempDir()
	agents := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# Top\n\nbefore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(target(t, "agents"), ScopeProject, root, "1.0.0", false); err != nil {
		t.Fatal(err)
	}
	withTail := readFile(t, agents) + "\n## Afterwards\n\nafter\n"
	if err := os.WriteFile(agents, []byte(withTail), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(target(t, "agents"), ScopeProject, root, "2.0.0", true); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, agents)
	for _, want := range []string{"before", "after", "## Afterwards"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q when updating the block", want)
		}
	}
	if strings.Index(got, "before") > strings.Index(got, beginMarker) {
		t.Error("ordering changed: user content should stay above the block")
	}
}

func TestUninstallLeavesUserContentBehind(t *testing.T) {
	root := t.TempDir()
	agents := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# Mine\n\nkeep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(target(t, "agents"), ScopeProject, root, "1.0.0", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(target(t, "agents"), ScopeProject, root); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, agents)
	if !strings.Contains(got, "keep me") {
		t.Error("uninstall destroyed the user's own instructions")
	}
	if strings.Contains(got, beginMarker) {
		t.Error("the managed block should be gone")
	}
}

// A file that held nothing but our block was ours, so it goes away entirely.
func TestUninstallRemovesAFileWeCreated(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(target(t, "agents"), ScopeProject, root, "1.0.0", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(target(t, "agents"), ScopeProject, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(err) {
		t.Error("a file containing only our block should be removed")
	}
}

func TestInspectDistinguishesOutdatedFromEdited(t *testing.T) {
	root := t.TempDir()
	tg := target(t, "cursor")
	if _, err := Install(tg, ScopeProject, root, "1.0.0", false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".cursor", "rules", Name+".mdc")

	st, err := Inspect(tg, ScopeProject, root, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Current || st.Modified || st.Outdated {
		t.Fatalf("a fresh install should be current, got %+v", st)
	}

	// Hand-edit the managed content.
	edited := readFile(t, path) + "\nlocal note\n"
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = Inspect(tg, ScopeProject, root, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Modified {
		t.Error("a hand edit should be reported as modified, not outdated")
	}
	if st.Outdated {
		t.Error("an edited install must not also read as outdated")
	}
}

func TestInstallRefusesToClobberEditsWithoutForce(t *testing.T) {
	root := t.TempDir()
	tg := target(t, "cursor")
	if _, err := Install(tg, ScopeProject, root, "1.0.0", false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".cursor", "rules", Name+".mdc")
	if err := os.WriteFile(path, []byte(readFile(t, path)+"\nmine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(tg, ScopeProject, root, "1.0.0", false); err == nil {
		t.Fatal("install should refuse to discard local edits")
	}
	if !strings.Contains(readFile(t, path), "mine") {
		t.Error("the edit was destroyed despite the refusal")
	}
	if _, err := Install(tg, ScopeProject, root, "1.0.0", true); err != nil {
		t.Fatalf("--force should overwrite: %v", err)
	}
	if strings.Contains(readFile(t, path), "mine") {
		t.Error("--force should have replaced the content")
	}
}

func TestBodyStripsClaudeFrontmatter(t *testing.T) {
	body := Body()
	if strings.HasPrefix(body, "---") {
		t.Error("the body should not start with YAML frontmatter")
	}
	if !strings.Contains(body, "# Upstacked CLI") {
		t.Error("the body should retain the document heading")
	}
	if strings.Contains(body, "description: Operate Upstacked") {
		t.Error("frontmatter leaked into the body")
	}
}

func TestClaudeTargetKeepsItsFrontmatter(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(target(t, "claude"), ScopeProject, root, "1.0.0", false); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(root, ".claude", "skills", Name, "SKILL.md"))
	if !strings.Contains(got, "name: "+Name) {
		t.Error("Claude Code needs the skill frontmatter")
	}
	// The regression that made the skill's description render as the marker.
	if !strings.HasPrefix(got, "---\nname: "+Name) {
		t.Fatalf("SKILL.md must begin with its frontmatter, got:\n%s", firstLines(got, 3))
	}
	fmEnd := strings.Index(got[4:], "\n---\n")
	if fmEnd < 0 {
		t.Fatal("frontmatter is not terminated")
	}
	if strings.Contains(got[:fmEnd], beginMarker) {
		t.Error("the marker must not sit inside the frontmatter")
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func TestResolveTargets(t *testing.T) {
	got, err := ResolveTargets([]string{"claude,agents"}, ScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 targets, got %d", len(got))
	}
	if _, err := ResolveTargets([]string{"nonesuch"}, ScopeProject); err == nil {
		t.Error("an unknown client should be rejected, not ignored")
	}
	// A client with no location at this scope must say so rather than being
	// silently dropped.
	if _, err := ResolveTargets([]string{"cursor"}, ScopeUser); err == nil {
		t.Error("cursor has no user scope and should be rejected")
	}
	all, err := ResolveTargets([]string{"all"}, ScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 5 {
		t.Errorf("'all' should expand to every project-scope client, got %d", len(all))
	}
}

// Every registered target must actually be installable, or the picker offers
// something that cannot work.
func TestEveryTargetInstallsAndUninstalls(t *testing.T) {
	for _, tg := range Targets {
		if !tg.Supports(ScopeProject) {
			continue
		}
		t.Run(tg.ID, func(t *testing.T) {
			root := t.TempDir()
			st, err := Install(tg, ScopeProject, root, "1.0.0", false)
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			if !st.Current {
				t.Errorf("expected current, got %s", st.Summary())
			}
			if _, err := os.Stat(st.Path); err != nil {
				t.Errorf("nothing written: %v", err)
			}
			if _, err := Uninstall(tg, ScopeProject, root); err != nil {
				t.Errorf("uninstall: %v", err)
			}
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Running from the home directory makes user and project scope resolve to the
// same file. Reporting it twice would imply two installs that can drift apart.
func TestInstalledStatesDeduplicatesByPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)

	if _, err := Install(target(t, "claude"), ScopeUser, "", "1.0.0", false); err != nil {
		t.Fatal(err)
	}
	states := InstalledStates("", "1.0.0")

	seen := map[string]int{}
	for _, st := range states {
		seen[st.Path]++
	}
	for path, n := range seen {
		if n > 1 {
			t.Errorf("%s reported %d times; the same file must appear once", path, n)
		}
	}
	if len(states) != 1 {
		t.Errorf("expected exactly one install, got %d", len(states))
	}
}
