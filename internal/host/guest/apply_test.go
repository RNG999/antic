package guest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBootSignature: systems differ in what they boot exactly when their
// kernel, initrd or kernel parameters differ (a switch cannot change them,
// so such a change restarts a running guest).
func TestBootSignature(t *testing.T) {
	root := t.TempDir()
	real := func(p string) string { return filepath.Join(root, p) }
	system := func(name, kernel, initrd, params string) string {
		dir := "/nix/store/" + name
		if err := os.MkdirAll(real(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		for link, target := range map[string]string{"kernel": kernel, "initrd": initrd} {
			if err := os.Symlink(target, real(dir+"/"+link)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(real(dir+"/kernel-params"), []byte(params), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	base := system("a-system", "/nix/store/k1/bzImage", "/nix/store/i1/initrd", "console=ttyS0")
	same := system("b-system", "/nix/store/k1/bzImage", "/nix/store/i1/initrd", "console=ttyS0")
	if bootSignature(real, base) != bootSignature(real, same) {
		t.Fatal("systems with the same kernel, initrd and parameters boot differently")
	}
	for _, other := range []string{
		system("c-system", "/nix/store/k2/bzImage", "/nix/store/i1/initrd", "console=ttyS0"),
		system("d-system", "/nix/store/k1/bzImage", "/nix/store/i2/initrd", "console=ttyS0"),
		system("e-system", "/nix/store/k1/bzImage", "/nix/store/i1/initrd", "console=hvc0"),
	} {
		if bootSignature(real, base) == bootSignature(real, other) {
			t.Fatalf("%s boots like %s", other, base)
		}
	}
}
