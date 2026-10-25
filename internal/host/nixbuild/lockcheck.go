package nixbuild

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Guest flakes often pin antic (or other inputs) to a local checkout. The
// lock records that checkout's narHash at lock time, so later edits would
// be silently ignored until the lock is updated.

type localInput struct {
	name, ref, path, lockedHash string
}

func flakeMetadata(ref string) (map[string]any, error) {
	out, err := exec.Command("nix", "flake", "metadata", "--json", "--no-write-lock-file", ref).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("nix flake metadata %s: %s", ref, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(out, &m)
}

// localInputs lists the locked inputs of flake that refer to local paths.
func localInputs(flake string) ([]localInput, error) {
	meta, err := flakeMetadata("path:" + flake)
	if err != nil {
		return nil, err
	}
	locks, _ := meta["locks"].(map[string]any)
	nodes, _ := locks["nodes"].(map[string]any)
	var out []localInput
	for name, n := range nodes {
		node, _ := n.(map[string]any)
		locked, _ := node["locked"].(map[string]any)
		hash, _ := locked["narHash"].(string)
		switch locked["type"] {
		case "path":
			p, _ := locked["path"].(string)
			if !filepath.IsAbs(p) {
				p = filepath.Join(flake, p)
			}
			out = append(out, localInput{name, "path:" + p, p, hash})
		case "git":
			if u, _ := locked["url"].(string); strings.HasPrefix(u, "file://") {
				out = append(out, localInput{name, "git+" + u, strings.TrimPrefix(u, "file://"), hash})
			}
		}
	}
	return out, nil
}

// CheckLocks refuses local inputs inside one of avoid (e.g. shared
// directories, which a guest can write), and automatically updates the
// flake's lock if it pins an older state of a local input.
func CheckLocks(flake string, avoid []string) error {
	inputs, err := localInputs(flake)
	if err != nil {
		return err
	}
	var stale []string
	for _, in := range inputs {
		for _, a := range avoid {
			if in.path == a || strings.HasPrefix(in.path, a+"/") {
				return fmt.Errorf("input %q is %s, inside %s, which a guest can write; move it out", in.name, in.path, a)
			}
		}
		cur, err := flakeMetadata(in.ref)
		if err != nil {
			return err
		}
		locked, _ := cur["locked"].(map[string]any)
		if h, _ := locked["narHash"].(string); h != in.lockedHash {
			stale = append(stale, in.name)
		}
	}
	if len(stale) > 0 {
		args := append([]string{"flake", "update"}, stale...)
		args = append(args, "--flake", "path:"+flake)
		if out, err := exec.Command("nix", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("nix %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
