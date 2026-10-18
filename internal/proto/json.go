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
		"credAuthorizedKeys": CredAuthorizedKeys,
		"credHostKey":        CredHostKey,
		"storeTag":           StoreTag,
		"runDir":             RunDir,
		"auditPort":          AuditPort,
	}, "", "  ")
	return append(b, '\n'), err
}
