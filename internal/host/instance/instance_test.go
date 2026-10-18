package instance

import (
	"os"
	"path/filepath"
	"testing"
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
