package egress

import (
	"testing"
)

func TestDomainAllowed(t *testing.T) {
	// Case 1: Allow list only (Default Deny)
	pAllowOnly := &Proxy{cfg: Config{
		Allow: []string{"*.github.com", "anthropic.com"},
	}}
	if !pAllowOnly.domainAllowed("github.com") {
		t.Error("expected github.com allowed (apex matches wildcard)")
	}
	if !pAllowOnly.domainAllowed("api.github.com") {
		t.Error("expected api.github.com allowed (subdomain matches wildcard)")
	}
	if !pAllowOnly.domainAllowed("anthropic.com") {
		t.Error("expected anthropic.com allowed (exact match)")
	}
	if pAllowOnly.domainAllowed("api.anthropic.com") {
		t.Error("expected api.anthropic.com denied (no wildcard)")
	}
	if pAllowOnly.domainAllowed("google.com") {
		t.Error("expected google.com denied")
	}

	// Case 2: Deny list only (Default Allow)
	pDenyOnly := &Proxy{cfg: Config{
		Deny: []string{"evil.com", "*.internal.corp"},
	}}
	if pDenyOnly.domainAllowed("evil.com") {
		t.Error("expected evil.com denied")
	}
	if pDenyOnly.domainAllowed("sub.internal.corp") {
		t.Error("expected sub.internal.corp denied (wildcard match)")
	}
	if !pDenyOnly.domainAllowed("google.com") {
		t.Error("expected google.com allowed")
	}

	// Case 3: Allow + Deny list (Deny takes priority)
	pBoth := &Proxy{cfg: Config{
		Allow: []string{"*.github.com"},
		Deny:  []string{"gist.github.com"},
	}}
	if !pBoth.domainAllowed("github.com") {
		t.Error("expected github.com allowed")
	}
	if !pBoth.domainAllowed("api.github.com") {
		t.Error("expected api.github.com allowed")
	}
	if pBoth.domainAllowed("gist.github.com") {
		t.Error("expected gist.github.com denied")
	}
	if pBoth.domainAllowed("google.com") {
		t.Error("expected google.com denied (not in allow list)")
	}
}
