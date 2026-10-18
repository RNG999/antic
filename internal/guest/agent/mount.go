package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/RNG999/hanare/internal/proto"
)

var shareTag = regexp.MustCompile(proto.ShareTagPattern)

// mount is a share mounted by the agent. root keeps the mount itself (an
// O_PATH fd from fsmount), so unmounting never resolves a path again.
type mount struct {
	path string
	root *os.File
}

// mount mounts virtiofs tag at path, which must lie below the guest user's
// home. Missing directories are created as the user. Nothing is resolved
// through symlinks or other mounts, so the user cannot redirect the mount
// onto a system path, and a tag is never mounted twice (a second mount
// would bypass this agent's checks).
func (a *Agent) mount(tag, path string, readOnly bool) error {
	if !shareTag.MatchString(tag) {
		return fmt.Errorf("invalid share tag %q", tag)
	}
	if _, ok := a.mounts[tag]; ok {
		return fmt.Errorf("share %s is already mounted", tag)
	}
	if mounted, err := tagMounted(tag); err != nil || mounted {
		return fmt.Errorf("share %s is already mounted (%v)", tag, err)
	}
	target, err := a.openTarget(path)
	if err != nil {
		return err
	}
	defer target.Close()

	fsfd, err := createVirtiofs(tag)
	if err != nil {
		return err
	}
	defer unix.Close(fsfd)
	attrs := unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV
	if readOnly {
		attrs |= unix.MOUNT_ATTR_RDONLY
	}
	mfd, err := unix.Fsmount(fsfd, unix.FSMOUNT_CLOEXEC, attrs)
	if err != nil {
		return fmt.Errorf("fsmount: %w", err)
	}
	if err := unix.MoveMount(mfd, "", int(target.Fd()), "",
		unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		unix.Close(mfd)
		return fmt.Errorf("move_mount to %s: %w", path, err)
	}
	a.mounts[tag] = &mount{path: path, root: os.NewFile(uintptr(mfd), tag)}
	return nil
}

// createVirtiofs prepares a virtiofs superblock for tag. A just hot-plugged
// device takes a moment to appear in the guest (until then the tag is
// unknown: EINVAL), so this retries for a while.
func createVirtiofs(tag string) (int, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		fsfd, err := unix.Fsopen("virtiofs", unix.FSOPEN_CLOEXEC)
		if err != nil {
			return -1, fmt.Errorf("fsopen virtiofs: %w", err)
		}
		if err = unix.FsconfigSetString(fsfd, "source", tag); err == nil {
			if err = unix.FsconfigCreate(fsfd); err == nil {
				return fsfd, nil
			}
		}
		unix.Close(fsfd)
		if !errors.Is(err, unix.EINVAL) || time.Now().After(deadline) {
			return -1, fmt.Errorf("virtiofs %s: %w", tag, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// unmount detaches exactly the mount made for tag.
func (a *Agent) unmount(tag string) error {
	m, ok := a.mounts[tag]
	if !ok {
		return fmt.Errorf("share %s was not mounted by this agent", tag)
	}
	// /proc/self/fd/N resolves to the mount's root, wherever it is now. Our
	// own fd keeps the mount busy, so detach it: it disappears from the
	// guest at once, and processes still using it see errors once the host
	// removes the device.
	if err := unix.Unmount("/proc/self/fd/"+strconv.Itoa(int(m.root.Fd())), unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount %s: %w", m.path, err)
	}
	m.root.Close()
	delete(a.mounts, tag)
	return nil
}

// openTarget opens path (an absolute path below the user's home) as a
// directory, creating missing components owned by the user. Each step is
// resolved without following symlinks or crossing mounts.
func (a *Agent) openTarget(path string) (*os.File, error) {
	clean := filepath.Clean(path)
	if clean != path || !strings.HasPrefix(path, a.home+"/") {
		return nil, fmt.Errorf("mount point %q is not a clean path below %s", path, a.home)
	}
	dir, err := unix.Open(a.home, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, comp := range strings.Split(strings.TrimPrefix(path, a.home+"/"), "/") {
		next, err := a.openDir(dir, comp)
		unix.Close(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		dir = next
	}
	return os.NewFile(uintptr(dir), path), nil
}

func (a *Agent) openDir(dir int, name string) (int, error) {
	how := &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_BENEATH | unix.RESOLVE_NO_XDEV,
	}
	fd, err := unix.Openat2(dir, name, how)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(dir, name, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, err
		}
		if err := unix.Fchownat(dir, name, a.uid, a.gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return -1, err
		}
		fd, err = unix.Openat2(dir, name, how)
	}
	return fd, err
}

// tagMounted reports whether a virtiofs mount of tag exists anywhere.
func tagMounted(tag string) (bool, error) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		// ... - fstype source options
		if i := strings.Index(line, " - "); i >= 0 {
			f := strings.Fields(line[i+3:])
			if len(f) >= 2 && f[0] == "virtiofs" && f[1] == tag {
				return true, nil
			}
		}
	}
	return false, nil
}
