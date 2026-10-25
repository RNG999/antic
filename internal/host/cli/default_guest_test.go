package cli

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/instance"
)

func setupTestAppWithGuests(t *testing.T, guestNames ...string) *app {
	t.Helper()
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "guests")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}

	for _, name := range guestNames {
		content := "memory = \"2G\"\ncpus = 2\nnetwork = \"internet\"\n"
		if err := os.WriteFile(filepath.Join(configDir, name+".toml"), []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write guest file: %v", err)
		}
	}

	u, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}

	cfg, err := config.Load(configDir, "/home/"+u.Username)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	d := instance.Dirs{
		ConfigDir: filepath.Dir(configDir),
		StateRoot: filepath.Join(tmp, "state"),
		RunRoot:   filepath.Join(tmp, "run"),
		Home:      "/home/" + u.Username,
	}

	return &app{
		dirs: d,
		cfg:  cfg,
	}
}

func TestResolveTargetGuest(t *testing.T) {
	ctx := context.Background()

	t.Run("zero guests", func(t *testing.T) {
		a := setupTestAppWithGuests(t)
		_, _, err := a.resolveTargetGuest(ctx, nil, "antic guest show [NAME]")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "no guests defined") || !strings.Contains(err.Error(), "antic guest create") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("single guest auto selected", func(t *testing.T) {
		a := setupTestAppWithGuests(t, "dev")
		name, rest, err := a.resolveTargetGuest(ctx, nil, "antic guest show [NAME]")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "dev" {
			t.Errorf("expected dev, got %s", name)
		}
		if len(rest) != 0 {
			t.Errorf("expected empty rest, got %v", rest)
		}
	})

	t.Run("single guest explicit name", func(t *testing.T) {
		a := setupTestAppWithGuests(t, "dev")
		name, rest, err := a.resolveTargetGuest(ctx, []string{"dev", "extra"}, "antic guest show [NAME]")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "dev" {
			t.Errorf("expected dev, got %s", name)
		}
		if len(rest) != 1 || rest[0] != "extra" {
			t.Errorf("expected [extra], got %v", rest)
		}
	})

	t.Run("multiple guests omitted name", func(t *testing.T) {
		a := setupTestAppWithGuests(t, "alpha", "beta")
		_, _, err := a.resolveTargetGuest(ctx, nil, "antic guest show [NAME]")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "guest NAME is required") {
			t.Errorf("unexpected error: %v", err)
		}
		if !strings.Contains(err.Error(), "alpha, beta") {
			t.Errorf("error should list available guests: %v", err)
		}
	})
}

func TestResolveGuestAndItems(t *testing.T) {
	ctx := context.Background()

	t.Run("zero guests", func(t *testing.T) {
		a := setupTestAppWithGuests(t)
		_, _, err := a.resolveGuestAndItems(ctx, []string{"ripgrep"}, "antic guest install [NAME] PKG...", "PKG")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "no guests defined") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("single guest with items omitted guest name", func(t *testing.T) {
		a := setupTestAppWithGuests(t, "dev")
		name, items, err := a.resolveGuestAndItems(ctx, []string{"ripgrep", "fd"}, "antic guest install [NAME] PKG...", "PKG")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "dev" {
			t.Errorf("expected dev, got %s", name)
		}
		if len(items) != 2 || items[0] != "ripgrep" || items[1] != "fd" {
			t.Errorf("unexpected items: %v", items)
		}
	})

	t.Run("single guest with items explicit guest name", func(t *testing.T) {
		a := setupTestAppWithGuests(t, "dev")
		name, items, err := a.resolveGuestAndItems(ctx, []string{"dev", "ripgrep"}, "antic guest install [NAME] PKG...", "PKG")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "dev" {
			t.Errorf("expected dev, got %s", name)
		}
		if len(items) != 1 || items[0] != "ripgrep" {
			t.Errorf("unexpected items: %v", items)
		}
	})

	t.Run("single guest without items", func(t *testing.T) {
		a := setupTestAppWithGuests(t, "dev")
		_, _, err := a.resolveGuestAndItems(ctx, nil, "antic guest install [NAME] PKG...", "PKG")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "syntax error: PKG is required") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("multiple guests omitted guest name", func(t *testing.T) {
		a := setupTestAppWithGuests(t, "alpha", "beta")
		_, _, err := a.resolveGuestAndItems(ctx, []string{"ripgrep"}, "antic guest install [NAME] PKG...", "PKG")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "guest NAME is required") {
			t.Errorf("unexpected error: %v", err)
		}
		if !strings.Contains(err.Error(), "alpha, beta") {
			t.Errorf("error should list available guests: %v", err)
		}
	})

	t.Run("multiple guests explicit guest name", func(t *testing.T) {
		a := setupTestAppWithGuests(t, "alpha", "beta")
		name, items, err := a.resolveGuestAndItems(ctx, []string{"beta", "ripgrep"}, "antic guest install [NAME] PKG...", "PKG")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "beta" {
			t.Errorf("expected beta, got %s", name)
		}
		if len(items) != 1 || items[0] != "ripgrep" {
			t.Errorf("unexpected items: %v", items)
		}
	})
}
