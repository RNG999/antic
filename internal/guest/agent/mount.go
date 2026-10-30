package agent

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/tea/control"
)

// mount mounts virtiofs tag at path, which the pure TEA control.Update has
// already verified lies strictly below the guest user's home. Missing
// directories are created as the user without following symlinks or crossing
// mounts.
func (a *Agent) mount(tag domain.ShareTag, path domain.AbsPath, readOnly bool) error {
	target, err := a.openTarget(path)
	if err != nil {
		return err
	}
	defer target.Close()

	fsfd, err := createVirtiofs(tag.Get())
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
		return fmt.Errorf("move_mount to %s: %w", path.Get(), err)
	}
	rfd, err := domain.ParseMountFD(mfd)
	if err != nil {
		unix.Close(mfd)
		return err
	}
	a.files[tag] = os.NewFile(uintptr(mfd), tag.Get())
	a.model, _ = control.Update(a.model, control.MsgMountCompleted{
		Tag:      tag,
		Path:     path,
		FD:       rfd,
		ReadOnly: readOnly,
	})
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

// unmount detaches the mount made for tag.
func (a *Agent) unmount(tag domain.ShareTag, path domain.AbsPath, fd domain.MountFD) error {
	if err := unix.Unmount("/proc/self/fd/"+strconv.Itoa(fd.Get()), unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount %s: %w", path.Get(), err)
	}
	if f, ok := a.files[tag]; ok {
		f.Close()
		delete(a.files, tag)
	}
	a.model, _ = control.Update(a.model, control.MsgUnmountCompleted{Tag: tag})
	return nil
}

// openTarget opens path (a validated AbsPath strictly below the user's home) as
// a directory, creating missing components owned by the user. Each step is
// resolved without following symlinks or crossing mounts.
func (a *Agent) openTarget(path domain.AbsPath) (*os.File, error) {
	home := a.model.Home().Get()
	rawPath := path.Get()
	dir, err := unix.Open(home, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, comp := range strings.Split(strings.TrimPrefix(rawPath, home+"/"), "/") {
		next, err := a.openDir(dir, comp)
		unix.Close(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rawPath, err)
		}
		dir = next
	}
	return os.NewFile(uintptr(dir), rawPath), nil
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
