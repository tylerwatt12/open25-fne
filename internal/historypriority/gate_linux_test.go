//go:build linux

package historypriority

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeGateUsesPrivateNamespaceWithoutChangingGroupParent(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0770); err != nil {
		t.Fatal(err)
	}
	turn, err := acquireRuntime(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if err = turn.Release(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		parent: 0770,
		filepath.Join(parent, "open25-fne-history"):                  0700,
		filepath.Join(parent, "open25-fne-history", "mutation.lock"): 0600,
	} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("unexpected runtime permissions for %s: info=%v want=%o error=%v", path, info, want, err)
		}
	}
	if _, err = os.Lstat(filepath.Join(parent, "open25-fne-history.lock")); !os.IsNotExist(err) {
		t.Fatalf("created a second/legacy scheduling path: %v", err)
	}
	// Reopening the same current namespace must not require a migration.
	turn, err = acquireRuntime(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if err = turn.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeGateRefusesExistingInsecureOrIndirectPathsWithoutRepair(t *testing.T) {
	for _, kind := range []string{"shared child", "child symlink", "child file", "parent symlink", "world writable parent"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			private := filepath.Join(parent, "open25-fne-history")
			check := parent
			want := os.FileMode(0700)
			switch kind {
			case "shared child":
				if err := os.Mkdir(private, 0770); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(private, 0770); err != nil {
					t.Fatal(err)
				}
				check, want = private, 0770
			case "child symlink":
				if err := os.Symlink(t.TempDir(), private); err != nil {
					t.Fatal(err)
				}
				check, want = private, 0777
			case "child file":
				if err := os.WriteFile(private, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
				check, want = private, 0600
			case "parent symlink":
				link := filepath.Join(t.TempDir(), "runtime-link")
				if err := os.Symlink(parent, link); err != nil {
					t.Fatal(err)
				}
				parent, check, want = link, link, 0777
			case "world writable parent":
				if err := os.Chmod(parent, 0777); err != nil {
					t.Fatal(err)
				}
				want = 0777
			}
			if turn, err := acquireRuntime(context.Background(), parent); err == nil {
				_ = turn.Release()
				t.Fatal("accepted an insecure or indirect runtime path")
			}
			info, err := os.Lstat(check)
			if err != nil || info.Mode().Perm() != want {
				t.Fatalf("refusal repaired/replaced the existing path: info=%v want=%o error=%v", info, want, err)
			}
			if kind == "child file" {
				if content, err := os.ReadFile(private); err != nil || string(content) != "preserve" {
					t.Fatal("refusal changed the existing file", err)
				}
			}
		})
	}
}
