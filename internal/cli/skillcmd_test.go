package cli

import (
	"errors"
	"testing"
)

func TestResolveAgentsRaw(t *testing.T) {
	t.Run("explicit flag skips prompt", func(t *testing.T) {
		called := false
		got, err := resolveAgentsRaw("popular", true, true, func(label, def string) (string, error) {
			called = true
			return "", nil
		})
		if err != nil {
			t.Fatalf("resolveAgentsRaw: %v", err)
		}
		if got != "popular" {
			t.Fatalf("expected popular, got %q", got)
		}
		if called {
			t.Fatal("prompt should not be called")
		}
	})

	t.Run("non-interactive skips prompt", func(t *testing.T) {
		called := false
		got, err := resolveAgentsRaw("popular", false, false, func(label, def string) (string, error) {
			called = true
			return "", nil
		})
		if err != nil {
			t.Fatalf("resolveAgentsRaw: %v", err)
		}
		if got != "popular" {
			t.Fatalf("expected popular, got %q", got)
		}
		if called {
			t.Fatal("prompt should not be called")
		}
	})

	t.Run("interactive prompt value is used", func(t *testing.T) {
		got, err := resolveAgentsRaw("popular", false, true, func(label, def string) (string, error) {
			return "claude,cursor", nil
		})
		if err != nil {
			t.Fatalf("resolveAgentsRaw: %v", err)
		}
		if got != "claude,cursor" {
			t.Fatalf("expected claude,cursor, got %q", got)
		}
	})

	t.Run("blank prompt keeps default", func(t *testing.T) {
		got, err := resolveAgentsRaw("popular", false, true, func(label, def string) (string, error) {
			return "   ", nil
		})
		if err != nil {
			t.Fatalf("resolveAgentsRaw: %v", err)
		}
		if got != "popular" {
			t.Fatalf("expected popular, got %q", got)
		}
	})

	t.Run("prompt error is returned", func(t *testing.T) {
		want := errors.New("boom")
		_, err := resolveAgentsRaw("popular", false, true, func(label, def string) (string, error) {
			return "", want
		})
		if !errors.Is(err, want) {
			t.Fatalf("expected %v, got %v", want, err)
		}
	})
}

