package instance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDirs(t *testing.T) Dirs {
	d := t.TempDir()
	return Dirs{Home: d, StateRoot: filepath.Join(d, "state"), RunRoot: filepath.Join(d, "run")}
}

func TestAssignLookupRemove(t *testing.T) {
	d := testDirs(t)
	if _, ok, err := d.Lookup("dev"); ok || err != nil {
		t.Fatalf("Lookup before Assign: %v %v", ok, err)
	}
	inst, err := d.Assign("dev")
	if err != nil {
		t.Fatal(err)
	}
	ro := filepath.Join(inst.StateDir(), "store/nix/store/x")
	os.MkdirAll(ro, 0o755)
	os.WriteFile(filepath.Join(ro, "f"), nil, 0o444)
	os.Chmod(ro, 0o555)
	got, ok, err := d.Lookup("dev")
	if !ok || err != nil || got.ID != inst.ID {
		t.Fatalf("Lookup = %+v %v %v", got, ok, err)
	}
	if err := d.Remove(got); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inst.StateDir()); !os.IsNotExist(err) {
		t.Fatalf("state dir still there: %v", err)
	}
}

func TestLookupRejectsBadLinks(t *testing.T) {
	d := testDirs(t)
	os.MkdirAll(d.namesDir(), 0o700)
	for _, dst := range []string{"..", "/", "../guests/../..", "../guests/xyz"} {
		os.Remove(filepath.Join(d.namesDir(), "bad"))
		os.Symlink(dst, filepath.Join(d.namesDir(), "bad"))
		if _, _, err := d.Lookup("bad"); err == nil {
			t.Errorf("Lookup accepted link to %q", dst)
		}
	}
	if err := d.Remove(Instance{Name: "x", ID: "..", d: d}); err == nil {
		t.Error("Remove accepted ID \"..\"")
	}
}

func TestOrphans(t *testing.T) {
	d := testDirs(t)
	a, _ := d.Assign("a")
	b, _ := d.Assign("a") // reassigning leaves the first ID orphaned
	o, err := d.Orphans()
	if err != nil || len(o) != 1 || filepath.Base(o[0]) != a.ID || a.ID == b.ID {
		t.Fatalf("Orphans = %v, %v", o, err)
	}
}

func TestLockContention(t *testing.T) {
	d := testDirs(t)
	unlock1, err := d.Lock("dev")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock1()

	acquired := make(chan struct{})
	go func() {
		unlock2, err := d.LockWithReason("dev", "test dev")
		if err != nil {
			t.Errorf("second lock: %v", err)
			return
		}
		defer unlock2()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("second lock acquired while first is held")
	default:
	}

	unlock1()
	<-acquired
}

func TestLockNixBuild(t *testing.T) {
	d := testDirs(t)
	unlock1, err := d.LockNixBuild()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock1()

	acquired := make(chan struct{})
	go func() {
		unlock2, err := d.LockNixBuild()
		if err != nil {
			t.Errorf("second nix build lock: %v", err)
			return
		}
		defer unlock2()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("second nix build lock acquired while first is held")
	default:
	}

	unlock1()
	<-acquired
}

func TestLockTimeout(t *testing.T) {
	d := testDirs(t)
	unlock1, err := d.Lock("dev")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock1()

	_, err = d.LockWithTimeout("dev", "test dev", 120*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out waiting for lock") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestLockContextCancel(t *testing.T) {
	d := testDirs(t)
	unlock1, err := d.Lock("dev")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock1()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	_, err = d.LockWithContext(ctx, "dev", "test dev", 10*time.Second)
	if err == nil {
		t.Fatal("expected context canceled error, got nil")
	}
	if !strings.Contains(err.Error(), "interrupted waiting for lock") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TestTrashRestore: trashing and restoring an instance are inverse, and
// repeating either does nothing (recovery may repeat them).
func TestTrashRestore(t *testing.T) {
	d := testDirs(t)
	inst, err := d.Assign("dev")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(inst.StateDir(), "marker")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := d.TrashInstance(inst); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := d.Lookup("dev"); ok {
		t.Fatal("a trashed instance is still named")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a trashed instance's state is still in place")
	}
	for range 2 {
		if err := d.RestoreInstance(inst); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok, _ := d.Lookup("dev"); !ok || got.ID != inst.ID {
		t.Fatal("a restored instance is not named")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a restored instance lost its state: %v", err)
	}
	if err := d.TrashInstance(inst); err != nil {
		t.Fatal(err)
	}
	if err := d.EmptyTrash("dev"); err != nil {
		t.Fatal(err)
	}
	if err := d.RestoreInstance(inst); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := d.Lookup("dev"); ok {
		t.Fatal("an emptied trash was restored")
	}
}
