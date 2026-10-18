package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Guest flakes often pin hanare (or other inputs) to a local checkout. The
// lock records that checkout's narHash at lock time, so later edits are
// silently ignored until the lock is updated; and "hanare switch" builds
// inside the guest, which cannot read paths outside the shared flake dir.

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
			out = append(out, localInput{name, "path:" + p, p, hash})
		case "git":
			if u, _ := locked["url"].(string); strings.HasPrefix(u, "file://") {
				out = append(out, localInput{name, "git+" + u, strings.TrimPrefix(u, "file://"), hash})
			}
		}
	}
	return out, nil
}

// checkGuestFlake refuses to build with a stale lock on a local input and,
// for "switch", with local inputs the guest cannot read.
func checkGuestFlake(cfg *Config, forSwitch bool) error {
	inputs, err := localInputs(cfg.Guest.Flake)
	if err != nil {
		return err
	}
	var stale []string
	for _, in := range inputs {
		if forSwitch && !within(in.path, cfg.Guest.Flake) {
			return fmt.Errorf("input %q is the local path %s, which the guest cannot read during \"hanare switch\"; use \"hanare reinstall\", or point the input at a git URL", in.name, in.path)
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
		return fmt.Errorf("%s/flake.lock pins an older state of local input(s) %s; update with:\n  nix flake update %s --flake path:%s",
			cfg.Guest.Flake, strings.Join(stale, ", "), strings.Join(stale, " "), cfg.Guest.Flake)
	}
	return nil
}
