package proto

import "encoding/json"

// JSON renders the parts of the contract the NixOS module needs, as written
// to nix/proto.json (kept in sync by TestNixProtoJSON).
func JSON() ([]byte, error) {
	b, err := json.MarshalIndent(map[string]any{
		"version":            Version,
		"portSession":        PortSession,
		"portSessionOffline": PortSessionOffline,
		"portAgent":          PortAgent,
		"storeTag":           StoreTag,
		"runDir":             RunDir,
	}, "", "  ")
	return append(b, '\n'), err
}
