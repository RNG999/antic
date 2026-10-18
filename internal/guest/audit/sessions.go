package audit

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RNG999/hanare/internal/proto"
)

// sessionNames maps cgroup IDs (the inode number of the cgroup directory)
// to session unit names. Misses are cached for a while, so events from
// processes outside sessions do not trigger a rescan each time.
type sessionNames struct {
	known  map[uint64]string
	misses map[uint64]time.Time
}

func newSessionNames() *sessionNames {
	return &sessionNames{known: map[uint64]string{}, misses: map[uint64]time.Time{}}
}

func (s *sessionNames) lookup(id uint64) string {
	if name, ok := s.known[id]; ok {
		return name
	}
	if t, ok := s.misses[id]; ok && time.Since(t) < 10*time.Second {
		return ""
	}
	for _, slice := range proto.SessionSlices {
		entries, _ := os.ReadDir(slice)
		for _, e := range entries {
			var st syscall.Stat_t
			d := filepath.Join(slice, e.Name())
			if strings.HasSuffix(d, ".service") && syscall.Stat(d, &st) == nil {
				s.known[st.Ino] = e.Name()
			}
		}
	}
	if name, ok := s.known[id]; ok {
		return name
	}
	if len(s.misses) > 4096 {
		clear(s.misses)
	}
	s.misses[id] = time.Now()

	return ""
}
