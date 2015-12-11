package libfuse

import (
	"errors"
	"fmt"
	"os/exec"
	"path"
	"runtime"

	"bazil.org/fuse"
)

// Mounter defines interface for different mounting strategies
type Mounter interface {
	Dir() string
	Mount() (*fuse.Conn, error)
	Unmount() error
}

// DefaultMounter will only call fuse.Mount and fuse.Unmount directly
type DefaultMounter struct {
	dir string
}

// NewDefaultMounter creates a default mounter.
func NewDefaultMounter(dir string) DefaultMounter {
	return DefaultMounter{dir: dir}
}

// Mount uses default mount
func (m DefaultMounter) Mount() (*fuse.Conn, error) {
	return fuseMountDir(m.dir)
}

// Unmount uses default unmount
func (m DefaultMounter) Unmount() error {
	return fuse.Unmount(m.dir)
}

// Dir returns mount directory.
func (m DefaultMounter) Dir() string {
	return m.dir
}

// ForceMounter will try its best to get it a mount
type ForceMounter struct {
	dir string
}

// NewForceMounter creates a force mounter.
func NewForceMounter(dir string) ForceMounter {
	return ForceMounter{dir: dir}
}

// Mount tries to mount and then unmount, re-mount if unsuccessful
func (m ForceMounter) Mount() (*fuse.Conn, error) {
	c, err := fuseMountDir(m.dir)
	if err == nil {
		return c, nil
	}

	// Mount failed, let's try to unmount and then try mounting again, even
	// if unmounting errors here.
	m.Unmount()

	c, err = fuseMountDir(m.dir)
	return c, err
}

// Unmount tries to unmount normally and then force if unsuccessful
func (m ForceMounter) Unmount() (err error) {
	// Try unmount
	err = fuse.Unmount(m.dir)
	if err != nil {
		// Unmount failed, so let's try and force it.
		err = m.forceUnmount()
	}
	return
}

func (m ForceMounter) forceUnmount() (err error) {
	if runtime.GOOS == "darwin" {
		_, err = exec.Command("/usr/sbin/diskutil", "unmountDisk", "force", m.dir).Output()
	} else if runtime.GOOS == "linux" {
		_, err = exec.Command("umount", "-l", m.dir).Output()
	} else {
		err = errors.New("Forced unmount is not supported on this platform yet")
	}
	return
}

// Dir returns mount directory.
func (m ForceMounter) Dir() string {
	return m.dir
}

func fuseMountDir(dir string) (*fuse.Conn, error) {
	options, err := getPlatformSpecificMountOptions(dir)
	if err != nil {
		return nil, err
	}
	return fuse.Mount(dir, options...)
}

// volumeName returns the directory (base) name
func volumeName(dir string) (string, error) {
	volName := path.Base(dir)
	if volName == "." || volName == "/" {
		err := fmt.Errorf("Bad volume name: %v", volName)
		return "", err
	}
	return volName, nil
}
