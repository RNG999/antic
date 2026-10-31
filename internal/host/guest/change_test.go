package guest

import (
	"slices"
	"testing"

	"github.com/RNG999/antic/internal/host/config"
)

// TestLiveShares: an edit of a running guest's shares is made live unless a
// mounted share changes mode or a new share overlaps a mount.
func TestLiveShares(t *testing.T) {
	bt := &boot{Shares: []bootShare{
		{Share: config.Share{Path: "/home/u/A"}, Tag: "a"},                  // mounted at boot
		{Share: config.Share{Path: "/home/u/R"}, Tag: "r", Runtime: true},   // hot-plugged
		{Share: config.Share{Path: "/home/u/old"}, Tag: "o", Runtime: true}, // hot-plugged, dropped below
	}}
	cfg := func(ss ...config.Share) *config.Guest { return &config.Guest{Shares: ss} }
	for _, c := range []struct {
		name           string
		c              *config.Guest
		added, removed []string
		live           bool
	}{
		{"unchanged but one dropped", cfg(config.Share{Path: "/home/u/A"}, config.Share{Path: "/home/u/R"}), nil, []string{"/home/u/old"}, true},
		{"new share", cfg(config.Share{Path: "/home/u/A"}, config.Share{Path: "/home/u/R"}, config.Share{Path: "/home/u/old"}, config.Share{Path: "/home/u/N"}), []string{"/home/u/N"}, nil, true},
		{"boot share removed stays mounted", cfg(config.Share{Path: "/home/u/R"}, config.Share{Path: "/home/u/old"}), nil, nil, true},
		{"mode change", cfg(config.Share{Path: "/home/u/A", ReadOnly: true}), nil, nil, false},
		{"overlap with a mount", cfg(config.Share{Path: "/home/u/A"}, config.Share{Path: "/home/u/A/sub"}), nil, nil, false},
	} {
		added, removed, live := liveShares(bt, c.c)
		if live != c.live || (live && (!slices.Equal(added, c.added) || !slices.Equal(removed, c.removed))) {
			t.Errorf("%s: added %v, removed %v, live %v", c.name, added, removed, live)
		}
	}
}
