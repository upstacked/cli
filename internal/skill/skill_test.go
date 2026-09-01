package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAgents(t *testing.T) {
	agents, err := ParseAgents("popular")
	if err != nil {
		t.Fatalf("ParseAgents(popular): %v", err)
	}
	if len(agents) != len(PopularAgents()) {
		t.Fatalf("expected %d agents, got %d", len(PopularAgents()), len(agents))
	}

	agents, err = ParseAgents("claude,cursor,claude")
	if err != nil {
		t.Fatalf("ParseAgents(list): %v", err)
	}
	if len(agents) != 2 || agents[0] != AgentClaude || agents[1] != AgentCursor {
		t.Fatalf("unexpected parsed agents: %#v", agents)
	}

	if _, err := ParseAgents("unknown"); err == nil {
		t.Fatal("expected unknown agent to fail")
	}
}

func TestInstallManyPopularUserScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	states, err := InstallMany(ScopeUser, "", "test", false, nil)
	if err != nil {
		t.Fatalf("InstallMany: %v", err)
	}
	wantAgents := PopularAgents()
	if len(states) != len(wantAgents) {
		t.Fatalf("expected %d states, got %d", len(wantAgents), len(states))
	}
	for i, a := range wantAgents {
		st := states[i]
		if st.Agent != a {
			t.Fatalf("expected state %d agent %q, got %q", i, a, st.Agent)
		}
		if !st.Current || !st.Installed {
			t.Fatalf("expected installed current state for %q", a)
		}
		wantPath := filepath.Join(home, "."+string(a), "skills", Name, "SKILL.md")
		if st.Path != wantPath {
			t.Fatalf("expected path %q, got %q", wantPath, st.Path)
		}
		b, err := os.ReadFile(st.Path)
		if err != nil {
			t.Fatalf("read %q: %v", st.Path, err)
		}
		if string(b) != Content {
			t.Fatalf("unexpected content in %q", st.Path)
		}
	}
}

