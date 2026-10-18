package proto

import (
	"bytes"
	"os"
	"testing"
)

// The NixOS module reads nix/proto.json; regenerate it with
// HANARE_UPDATE_PROTO=1 go test ./internal/proto.
func TestNixProtoJSON(t *testing.T) {
	want, err := JSON()
	if err != nil {
		t.Fatal(err)
	}
	const path = "../../nix/proto.json"
	if os.Getenv("HANARE_UPDATE_PROTO") != "" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date; run HANARE_UPDATE_PROTO=1 go test ./internal/proto", path)
	}
}
