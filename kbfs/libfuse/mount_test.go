package libfuse

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
	"github.com/keybase/kbfs/libkbfs"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
)

func makeFS(t testing.TB, config *libkbfs.ConfigLocal) (
	*fstestutil.Mount, *FS, func()) {
	log := logger.NewTestLogger(t)
	fuse.Debug = func(msg interface{}) {
		log.Debug("%s", msg)
	}

	// TODO duplicates main() in kbfsfuse/main.go too much
	filesys := &FS{
		config: config,
		log:    logger.NewTestLogger(t),
	}
	ctx := context.Background()
	ctx = context.WithValue(ctx, CtxAppIDKey, filesys)
	ctx, cancelFn := context.WithCancel(ctx)
	fn := func(mnt *fstestutil.Mount) fs.FS {
		filesys.fuse = mnt.Server
		filesys.conn = mnt.Conn
		return filesys
	}
	mnt, err := fstestutil.MountedFuncT(t, fn, &fs.Config{
		GetContext: func() context.Context {
			return ctx
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	filesys.launchNotificationProcessor(ctx)
	return mnt, filesys, cancelFn
}

type fileInfoCheck func(fi os.FileInfo) error

func mustBeDir(fi os.FileInfo) error {
	if !fi.IsDir() {
		return fmt.Errorf("not a directory: %v", fi)
	}
	return nil
}

func checkDir(t testing.TB, dir string, want map[string]fileInfoCheck) {
	// make a copy of want, to be safe
	{
		tmp := make(map[string]fileInfoCheck, len(want))
		for k, v := range want {
			tmp[k] = v
		}
		want = tmp
	}

	fis, err := ioutil.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, fi := range fis {
		if check, ok := want[fi.Name()]; ok {
			delete(want, fi.Name())
			if check != nil {
				if err := check(fi); err != nil {
					t.Errorf("check failed: %v: %v", fi.Name(), err)
				}
			}
			continue
		}
		t.Errorf("unexpected direntry: %q size=%v mode=%v", fi.Name(), fi.Size(), fi.Mode())
	}
	for filename := range want {
		t.Errorf("never saw file: %v", filename)
	}
}

// fsTimeEqual compares two filesystem-related timestamps.
//
// On platforms that don't use nanosecond-accurate timestamps in their
// filesystem APIs, it truncates the timestamps to make them
// comparable.
func fsTimeEqual(a, b time.Time) bool {
	if runtime.GOOS == "darwin" {
		a = a.Truncate(1 * time.Second)
		b = b.Truncate(1 * time.Second)
	}
	return a == b
}

// timeEqualFuzzy returns whether a is b+-skew.
func timeEqualFuzzy(a, b time.Time, skew time.Duration) bool {
	b1 := b.Add(-skew)
	b2 := b.Add(skew)
	return !a.Before(b1) && !a.After(b2)
}

func TestStatRoot(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	fi, err := os.Lstat(mnt.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Mode().String(), `drwxr-xr-x`; g != e {
		t.Errorf("wrong mode for folder: %q != %q", g, e)
	}
}

func TestStatPrivate(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	fi, err := os.Lstat(path.Join(mnt.Dir, PrivateName))
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Mode().String(), `drwxr-xr-x`; g != e {
		t.Errorf("wrong mode for folder: %q != %q", g, e)
	}
}

func TestStatPublic(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	fi, err := os.Lstat(path.Join(mnt.Dir, PublicName))
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Mode().String(), `drwxr-xr-x`; g != e {
		t.Errorf("wrong mode for folder: %q != %q", g, e)
	}
}

func TestStatMyFolder(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	fi, err := os.Lstat(path.Join(mnt.Dir, PrivateName, "jdoe"))
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Mode().String(), `drwx------`; g != e {
		t.Errorf("wrong mode for folder: %q != %q", g, e)
	}
}

func TestStatNonexistentFolder(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	if _, err := os.Lstat(path.Join(mnt.Dir, PrivateName, "does-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("expected ENOENT: %v", err)
	}
}

func TestStatAlias(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe,jdoe")
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Mode().String(), `Lrwxrwxrwx`; g != e {
		t.Errorf("wrong mode for alias : %q != %q", g, e)
	}
	target, err := os.Readlink(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := target, "jdoe"; g != e {
		t.Errorf("wrong alias symlink target: %q != %q", g, e)
	}
}

func TestStatMyPublic(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	fi, err := os.Lstat(path.Join(mnt.Dir, PublicName, "jdoe"))
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Mode().String(), `drwxr-xr-x`; g != e {
		t.Errorf("wrong mode for folder: %q != %q", g, e)
	}
}

func TestReaddirRoot(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	checkDir(t, mnt.Dir, map[string]fileInfoCheck{
		PrivateName: mustBeDir,
		PublicName:  mustBeDir,
	})
}

func TestReaddirPrivate(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	{
		// Force FakeMDServer to have some TlfIDs it can present to us
		// as favorites. Don't go through VFS to avoid caching causing
		// false positives.
		dh, err := libkbfs.ParseTlfHandle(context.Background(), config, "jdoe")
		if err != nil {
			t.Fatalf("cannot parse jdoe as folder: %v", err)
		}
		if _, _, err := config.KBFSOps().GetOrCreateRootNodeForHandle(
			context.Background(), dh, libkbfs.MasterBranch); err != nil {
			t.Fatalf("cannot set up a favorite: %v", err)
		}
	}

	checkDir(t, path.Join(mnt.Dir, PrivateName), map[string]fileInfoCheck{
		"jdoe": mustBeDir,
	})
}

func TestReaddirPublic(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	{
		// Force FakeMDServer to have some TlfIDs it can present to us
		// as favorites. Don't go through VFS to avoid caching causing
		// false positives.
		dh, err := libkbfs.ParseTlfHandle(context.Background(), config, "jdoe")
		dh.Readers = append(dh.Readers, keybase1.PublicUID)
		if err != nil {
			t.Fatalf("cannot parse jdoe as folder: %v", err)
		}
		if _, _, err := config.KBFSOps().GetOrCreateRootNodeForHandle(
			context.Background(), dh, libkbfs.MasterBranch); err != nil {
			t.Fatalf("cannot set up a favorite: %v", err)
		}
	}

	checkDir(t, path.Join(mnt.Dir, PublicName), map[string]fileInfoCheck{
		"jdoe": mustBeDir,
	})
}

func TestReaddirMyFolderEmpty(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe"), map[string]fileInfoCheck{})
}

func TestReaddirMyFolderWithFiles(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	files := map[string]fileInfoCheck{
		"one": nil,
		"two": nil,
	}
	for filename, check := range files {
		if check != nil {
			// only set up the files
			continue
		}
		if err := ioutil.WriteFile(path.Join(mnt.Dir, PrivateName, "jdoe", filename), []byte("data for "+filename), 0644); err != nil {
			t.Fatal(err)
		}
	}
	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe"), files)
}

func testOneCreateThenRead(t *testing.T, p string) {
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const input = "hello, world\n"
	if _, err := io.WriteString(f, input); err != nil {
		t.Fatalf("write error: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("error on close: %v", err)
	}

	buf, err := ioutil.ReadFile(p)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	if g, e := string(buf), input; g != e {
		t.Errorf("bad file contents: %q != %q", g, e)
	}
}

func TestCreateThenRead(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	testOneCreateThenRead(t, p)
}

// Tests that writing and reading multiple files works, implicitly
// exercising any block pointer reference counting code (since the
// initial created files will have identical empty blocks to start
// with).
func TestMultipleCreateThenRead(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p1 := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile1")
	testOneCreateThenRead(t, p1)
	p2 := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile2")
	testOneCreateThenRead(t, p2)
}

func TestReadUnflushed(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const input = "hello, world\n"
	if _, err := io.WriteString(f, input); err != nil {
		t.Fatalf("write error: %v", err)
	}
	// explicitly no close here

	buf, err := ioutil.ReadFile(p)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	if g, e := string(buf), input; g != e {
		t.Errorf("bad file contents: %q != %q", g, e)
	}
}

func TestMountAgain(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()

	const input = "hello, world\n"
	const filename = "myfile"
	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()

		p := path.Join(mnt.Dir, PrivateName, "jdoe", filename)
		if err := ioutil.WriteFile(p, []byte(input), 0644); err != nil {
			t.Fatal(err)
		}
	}()

	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()
		p := path.Join(mnt.Dir, PrivateName, "jdoe", filename)
		buf, err := ioutil.ReadFile(p)
		if err != nil {
			t.Fatalf("read error: %v", err)
		}
		if g, e := string(buf), input; g != e {
			t.Errorf("bad file contents: %q != %q", g, e)
		}
	}()
}

func TestCreateExecutable(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	if err := ioutil.WriteFile(p, []byte("fake binary"), 0755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Mode().String(), `-rwxr-xr-x`; g != e {
		t.Errorf("wrong mode for executable: %q != %q", g, e)
	}
}

func TestMkdir(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "mydir")
	if err := os.Mkdir(p, 0755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Mode().String(), `drwx------`; g != e {
		t.Errorf("wrong mode for subdir: %q != %q", g, e)
	}
}

func TestMkdirAndCreateDeep(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	const input = "hello, world\n"

	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()

		one := path.Join(mnt.Dir, PrivateName, "jdoe", "one")
		if err := os.Mkdir(one, 0755); err != nil {
			t.Fatal(err)
		}
		two := path.Join(one, "two")
		if err := os.Mkdir(two, 0755); err != nil {
			t.Fatal(err)
		}
		three := path.Join(two, "three")
		if err := ioutil.WriteFile(three, []byte(input), 0644); err != nil {
			t.Fatal(err)
		}
	}()

	// unmount to flush cache
	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()

		p := path.Join(mnt.Dir, PrivateName, "jdoe", "one", "two", "three")
		buf, err := ioutil.ReadFile(p)
		if err != nil {
			t.Fatalf("read error: %v", err)
		}
		if g, e := string(buf), input; g != e {
			t.Errorf("bad file contents: %q != %q", g, e)
		}
	}()
}

func TestSymlink(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()

	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()

		p := path.Join(mnt.Dir, PrivateName, "jdoe", "mylink")
		if err := os.Symlink("myfile", p); err != nil {
			t.Fatal(err)
		}
	}()

	// unmount to flush cache
	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()

		p := path.Join(mnt.Dir, PrivateName, "jdoe", "mylink")
		target, err := os.Readlink(p)
		if err != nil {
			t.Fatal(err)
		}
		if g, e := target, "myfile"; g != e {
			t.Errorf("bad symlink target: %q != %q", g, e)
		}
	}()
}

func TestRename(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p1 := path.Join(mnt.Dir, PrivateName, "jdoe", "old")
	p2 := path.Join(mnt.Dir, PrivateName, "jdoe", "new")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p1, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(p1, p2); err != nil {
		t.Fatal(err)
	}

	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe"), map[string]fileInfoCheck{
		"new": func(fi os.FileInfo) error {
			if fi.Size() != int64(len(input)) {
				return fmt.Errorf("Bad file size: %d", fi.Size())
			}
			return nil
		},
	})

	buf, err := ioutil.ReadFile(p2)
	if err != nil {
		t.Errorf("read error: %v", err)
	}
	if g, e := string(buf), input; g != e {
		t.Errorf("bad file contents: %q != %q", g, e)
	}

	if _, err := ioutil.ReadFile(p1); !os.IsNotExist(err) {
		t.Errorf("old name still exists: %v", err)
	}
}

func TestRenameOverwrite(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p1 := path.Join(mnt.Dir, PrivateName, "jdoe", "old")
	p2 := path.Join(mnt.Dir, PrivateName, "jdoe", "new")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p1, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(p2, []byte("loser\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(p1, p2); err != nil {
		t.Fatal(err)
	}

	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe"), map[string]fileInfoCheck{
		"new": nil,
	})

	buf, err := ioutil.ReadFile(p2)
	if err != nil {
		t.Errorf("read error: %v", err)
	}
	if g, e := string(buf), input; g != e {
		t.Errorf("bad file contents: %q != %q", g, e)
	}

	if _, err := ioutil.ReadFile(p1); !os.IsNotExist(err) {
		t.Errorf("old name still exists: %v", err)
	}
}

func TestRenameCrossDir(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	if err := os.Mkdir(path.Join(mnt.Dir, PrivateName, "jdoe", "one"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path.Join(mnt.Dir, PrivateName, "jdoe", "two"), 0755); err != nil {
		t.Fatal(err)
	}
	p1 := path.Join(mnt.Dir, PrivateName, "jdoe", "one", "old")
	p2 := path.Join(mnt.Dir, PrivateName, "jdoe", "two", "new")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p1, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(p1, p2); err != nil {
		t.Fatal(err)
	}

	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe", "one"), map[string]fileInfoCheck{})
	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe", "two"), map[string]fileInfoCheck{
		"new": nil,
	})

	buf, err := ioutil.ReadFile(p2)
	if err != nil {
		t.Errorf("read error: %v", err)
	}
	if g, e := string(buf), input; g != e {
		t.Errorf("bad file contents: %q != %q", g, e)
	}

	if _, err := ioutil.ReadFile(p1); !os.IsNotExist(err) {
		t.Errorf("old name still exists: %v", err)
	}
}

func TestRenameCrossFolder(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p1 := path.Join(mnt.Dir, PrivateName, "jdoe", "old")
	p2 := path.Join(mnt.Dir, PrivateName, "wsmith,jdoe", "new")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p1, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	err := os.Rename(p1, p2)
	if err == nil {
		t.Fatalf("expected an error from rename: %v", err)
	}
	lerr, ok := err.(*os.LinkError)
	if !ok {
		t.Fatalf("expected a LinkError from rename: %v", err)
	}
	if g, e := lerr.Op, "rename"; g != e {
		t.Errorf("wrong LinkError.Op: %q != %q", g, e)
	}
	if g, e := lerr.Old, p1; g != e {
		t.Errorf("wrong LinkError.Old: %q != %q", g, e)
	}
	if g, e := lerr.New, p2; g != e {
		t.Errorf("wrong LinkError.New: %q != %q", g, e)
	}
	if g, e := lerr.Err, syscall.EXDEV; g != e {
		t.Errorf("expected EXDEV: %T %v", lerr.Err, lerr.Err)
	}

	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe"), map[string]fileInfoCheck{
		"old": nil,
	})
	checkDir(t, path.Join(mnt.Dir, PrivateName, "wsmith,jdoe"), map[string]fileInfoCheck{})

	buf, err := ioutil.ReadFile(p1)
	if err != nil {
		t.Errorf("read error: %v", err)
	}
	if g, e := string(buf), input; g != e {
		t.Errorf("bad file contents: %q != %q", g, e)
	}

	if _, err := ioutil.ReadFile(p2); !os.IsNotExist(err) {
		t.Errorf("new name exists even on error: %v", err)
	}
}

func TestWriteThenRename(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p1 := path.Join(mnt.Dir, PrivateName, "jdoe", "old")
	p2 := path.Join(mnt.Dir, PrivateName, "jdoe", "new")

	f, err := os.Create(p1)
	if err != nil {
		t.Fatalf("cannot create file: %v", err)
	}
	defer f.Close()

	// write to the file
	const input = "hello, world\n"
	if _, err := f.Write([]byte(input)); err != nil {
		t.Fatalf("cannot write: %v", err)
	}

	// now rename the file while it's still open
	if err := os.Rename(p1, p2); err != nil {
		t.Fatal(err)
	}

	// check that the new path has the right length still
	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe"), map[string]fileInfoCheck{
		"new": func(fi os.FileInfo) error {
			if fi.Size() != int64(len(input)) {
				return fmt.Errorf("Bad file size: %d", fi.Size())
			}
			return nil
		},
	})

	// write again to the same file
	const input2 = "goodbye, world\n"
	if _, err := f.Write([]byte(input2)); err != nil {
		t.Fatalf("cannot write after rename: %v", err)
	}

	buf, err := ioutil.ReadFile(p2)
	if err != nil {
		t.Errorf("read error: %v", err)
	}
	if g, e := string(buf), input+input2; g != e {
		t.Errorf("bad file contents: %q != %q", g, e)
	}

	if _, err := ioutil.ReadFile(p1); !os.IsNotExist(err) {
		t.Errorf("old name still exists: %v", err)
	}
}

func TestWriteThenRenameCrossDir(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	if err := os.Mkdir(path.Join(mnt.Dir, PrivateName, "jdoe", "one"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path.Join(mnt.Dir, PrivateName, "jdoe", "two"), 0755); err != nil {
		t.Fatal(err)
	}
	p1 := path.Join(mnt.Dir, PrivateName, "jdoe", "one", "old")
	p2 := path.Join(mnt.Dir, PrivateName, "jdoe", "two", "new")

	f, err := os.Create(p1)
	if err != nil {
		t.Fatalf("cannot create file: %v", err)
	}
	defer f.Close()

	// write to the file
	const input = "hello, world\n"
	if _, err := f.Write([]byte(input)); err != nil {
		t.Fatalf("cannot write: %v", err)
	}

	// now rename the file while it's still open
	if err := os.Rename(p1, p2); err != nil {
		t.Fatal(err)
	}

	// check that the new path has the right length still
	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe", "two"), map[string]fileInfoCheck{
		"new": func(fi os.FileInfo) error {
			if fi.Size() != int64(len(input)) {
				return fmt.Errorf("Bad file size: %d", fi.Size())
			}
			return nil
		},
	})

	// write again to the same file
	const input2 = "goodbye, world\n"
	if _, err := f.Write([]byte(input2)); err != nil {
		t.Fatalf("cannot write after rename: %v", err)
	}

	buf, err := ioutil.ReadFile(p2)
	if err != nil {
		t.Errorf("read error: %v", err)
	}
	if g, e := string(buf), input+input2; g != e {
		t.Errorf("bad file contents: %q != %q", g, e)
	}

	if _, err := ioutil.ReadFile(p1); !os.IsNotExist(err) {
		t.Errorf("old name still exists: %v", err)
	}
}

func TestRemoveFile(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}

	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe"), map[string]fileInfoCheck{})

	if _, err := ioutil.ReadFile(p); !os.IsNotExist(err) {
		t.Errorf("file still exists: %v", err)
	}
}

func TestRemoveDir(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "mydir")
	if err := os.Mkdir(p, 0755); err != nil {
		t.Fatal(err)
	}

	if err := syscall.Rmdir(p); err != nil {
		t.Fatal(err)
	}

	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe"), map[string]fileInfoCheck{})

	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file still exists: %v", err)
	}
}

func TestRemoveDirNotEmpty(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "mydir")
	if err := os.Mkdir(p, 0755); err != nil {
		t.Fatal(err)
	}
	pFile := path.Join(p, "myfile")
	if err := ioutil.WriteFile(pFile, []byte("i'm important"), 0644); err != nil {
		t.Fatal(err)
	}

	err := syscall.Rmdir(p)
	if g, e := err, syscall.ENOTEMPTY; g != e {
		t.Fatalf("wrong error from rmdir: %v (%T) != %v (%T)", g, g, e, e)
	}

	if _, err := ioutil.ReadFile(pFile); err != nil {
		t.Errorf("file was lost: %v", err)
	}
}

func TestRemoveFileWhileOpenWriting(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("cannot create file: %v", err)
	}
	defer f.Close()

	if err := os.Remove(p); err != nil {
		t.Fatalf("cannot delete file: %v", err)
	}

	// this must not resurrect a deleted file
	const input = "hello, world\n"
	if _, err := f.Write([]byte(input)); err != nil {
		t.Fatalf("cannot write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("error on close: %v", err)
	}

	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe"), map[string]fileInfoCheck{})

	if _, err := ioutil.ReadFile(p); !os.IsNotExist(err) {
		t.Errorf("file still exists: %v", err)
	}
}

func TestRemoveFileWhileOpenReading(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("cannot open file: %v", err)
	}
	defer f.Close()

	if err := os.Remove(p); err != nil {
		t.Fatalf("cannot delete file: %v", err)
	}

	buf, err := ioutil.ReadAll(f)
	if err != nil {
		t.Fatalf("cannot read unlinked file: %v", err)
	}
	if g, e := string(buf), input; g != e {
		t.Errorf("read wrong content: %q != %q", g, e)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("error on close: %v", err)
	}

	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe"), map[string]fileInfoCheck{})

	if _, err := ioutil.ReadFile(p); !os.IsNotExist(err) {
		t.Errorf("file still exists: %v", err)
	}
}

func TestRemoveFileWhileOpenReadingAcrossMounts(t *testing.T) {
	config1 := libkbfs.MakeTestConfigOrBust(t, "user1",
		"user2")
	defer config1.Shutdown()
	mnt1, fs1, cancelFn1 := makeFS(t, config1)
	defer mnt1.Close()
	defer cancelFn1()

	config2 := libkbfs.ConfigAsUser(config1, "user2")
	defer config2.Shutdown()
	mnt2, _, cancelFn2 := makeFS(t, config2)
	defer mnt2.Close()
	defer cancelFn2()

	if !mnt2.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	p1 := path.Join(mnt1.Dir, PrivateName, "user1,user2", "myfile")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p1, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(p1)
	if err != nil {
		t.Fatalf("cannot open file: %v", err)
	}
	defer f.Close()

	p2 := path.Join(mnt2.Dir, PrivateName, "user1,user2", "myfile")
	if err := os.Remove(p2); err != nil {
		t.Fatalf("cannot delete file: %v", err)
	}

	syncFolderToServer(t, "user1,user2", fs1)

	buf, err := ioutil.ReadAll(f)
	if err != nil {
		t.Fatalf("cannot read unlinked file: %v", err)
	}
	if g, e := string(buf), input; g != e {
		t.Errorf("read wrong content: %q != %q", g, e)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("error on close: %v", err)
	}

	checkDir(t, path.Join(mnt1.Dir, PrivateName, "user1,user2"),
		map[string]fileInfoCheck{})

	if _, err := ioutil.ReadFile(p1); !os.IsNotExist(err) {
		t.Errorf("file still exists: %v", err)
	}
}

func TestRenameOverFileWhileOpenReadingAcrossMounts(t *testing.T) {
	config1 := libkbfs.MakeTestConfigOrBust(t, "user1",
		"user2")
	defer config1.Shutdown()
	mnt1, fs1, cancelFn1 := makeFS(t, config1)
	defer mnt1.Close()
	defer cancelFn1()

	config2 := libkbfs.ConfigAsUser(config1, "user2")
	defer config2.Shutdown()
	mnt2, _, cancelFn2 := makeFS(t, config2)
	defer mnt2.Close()
	defer cancelFn2()

	if !mnt2.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	p1 := path.Join(mnt1.Dir, PrivateName, "user1,user2", "myfile")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p1, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	p1Other := path.Join(mnt1.Dir, PrivateName, "user1,user2", "other")
	const inputOther = "hello, other\n"
	if err := ioutil.WriteFile(p1Other, []byte(inputOther), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(p1)
	if err != nil {
		t.Fatalf("cannot open file: %v", err)
	}
	defer f.Close()

	p2Other := path.Join(mnt2.Dir, PrivateName, "user1,user2", "other")
	p2 := path.Join(mnt2.Dir, PrivateName, "user1,user2", "myfile")
	if err := os.Rename(p2Other, p2); err != nil {
		t.Fatalf("cannot rename file: %v", err)
	}

	syncFolderToServer(t, "user1,user2", fs1)

	buf, err := ioutil.ReadAll(f)
	if err != nil {
		t.Fatalf("cannot read unlinked file: %v", err)
	}
	if g, e := string(buf), input; g != e {
		t.Errorf("read wrong content: %q != %q", g, e)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("error on close: %v", err)
	}

	checkDir(t, path.Join(mnt1.Dir, PrivateName, "user1,user2"),
		map[string]fileInfoCheck{
			"myfile": nil,
		})

	if _, err := ioutil.ReadFile(p1Other); !os.IsNotExist(err) {
		t.Errorf("other file still exists: %v", err)
	}

	buf, err = ioutil.ReadFile(p1)
	if err != nil {
		t.Errorf("read error: %v", err)
	}
	if g, e := string(buf), inputOther; g != e {
		t.Errorf("bad file contents: %q != %q", g, e)
	}
}

func TestTruncateGrow(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	const newSize = 100
	if err := os.Truncate(p, newSize); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Size(), int64(newSize); g != e {
		t.Errorf("wrong size: %v != %v", g, e)
	}

	buf, err := ioutil.ReadFile(p)
	if err != nil {
		t.Fatalf("cannot read unlinked file: %v", err)
	}
	if g, e := string(buf), input+strings.Repeat("\x00", newSize-len(input)); g != e {
		t.Errorf("read wrong content: %q != %q", g, e)
	}
}

func TestTruncateShrink(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	const newSize = 4
	if err := os.Truncate(p, newSize); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Size(), int64(newSize); g != e {
		t.Errorf("wrong size: %v != %v", g, e)
	}

	buf, err := ioutil.ReadFile(p)
	if err != nil {
		t.Fatalf("cannot read unlinked file: %v", err)
	}
	if g, e := string(buf), input[:newSize]; g != e {
		t.Errorf("read wrong content: %q != %q", g, e)
	}
}

func TestChmodExec(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(p, 0744); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Mode().String(), `-rwxr-xr-x`; g != e {
		t.Errorf("wrong mode: %q != %q", g, e)
	}
}

func TestChmodNonExec(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p, []byte(input), 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(p, 0655); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.Mode().String(), `-rw-r--r--`; g != e {
		t.Errorf("wrong mode: %q != %q", g, e)
	}
}

func TestChmodDir(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	if err := os.Mkdir(p, 0755); err != nil {
		t.Fatal(err)
	}

	switch err := os.Chmod(p, 0655); err := err.(type) {
	case *os.PathError:
		if g, e := err.Err, syscall.EPERM; g != e {
			t.Fatalf("wrong error: %v != %v", g, e)
		}
	default:
		t.Fatalf("expected a PathError, got %T: %v", err, err)
	}
}

func TestSetattrFileMtime(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	mtime := time.Date(2015, 1, 2, 3, 4, 5, 6, time.Local)
	// KBFS does not respect atime (which is ok), but we need to give
	// something to the syscall.
	atime := time.Date(2015, 7, 8, 9, 10, 11, 12, time.Local)
	if err := os.Chtimes(p, atime, mtime); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.ModTime(), mtime; !fsTimeEqual(g, e) {
		t.Errorf("wrong mtime: %v !~= %v", g, e)
	}
}

func TestSetattrFileMtimeNow(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	const input = "hello, world\n"
	if err := ioutil.WriteFile(p, []byte(input), 0644); err != nil {
		t.Fatal(err)
	}

	mtime := time.Date(2015, 1, 2, 3, 4, 5, 6, time.Local)
	// KBFS does not respect atime (which is ok), but we need to give
	// something to the syscall.
	atime := time.Date(2015, 7, 8, 9, 10, 11, 12, time.Local)
	if err := os.Chtimes(p, atime, mtime); err != nil {
		t.Fatal(err)
	}

	// cause mtime to be set to now
	if err := unix.Utimes(p, nil); err != nil {
		t.Fatalf("touch failed: %v", err)
	}
	now := time.Now()

	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, o := fi.ModTime(), mtime; !g.After(o) {
		t.Errorf("mtime did not progress: %v <= %v", g, o)
	}
	if g, e := fi.ModTime(), now; !timeEqualFuzzy(g, e, 1*time.Second) {
		t.Errorf("mtime is wrong: %v !~= %v", g, e)
	}
}

func TestSetattrDirMtime(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "mydir")
	if err := os.Mkdir(p, 0755); err != nil {
		t.Fatal(err)
	}

	mtime := time.Date(2015, 1, 2, 3, 4, 5, 6, time.Local)
	// KBFS does not respect atime (which is ok), but we need to give
	// something to the syscall.
	atime := time.Date(2015, 7, 8, 9, 10, 11, 12, time.Local)
	if err := os.Chtimes(p, atime, mtime); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := fi.ModTime(), mtime; !fsTimeEqual(g, e) {
		t.Errorf("wrong mtime: %v !~= %v", g, e)
	}
}

func TestSetattrDirMtimeNow(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "mydir")
	if err := os.Mkdir(p, 0755); err != nil {
		t.Fatal(err)
	}

	mtime := time.Date(2015, 1, 2, 3, 4, 5, 6, time.Local)
	// KBFS does not respect atime (which is ok), but we need to give
	// something to the syscall.
	atime := time.Date(2015, 7, 8, 9, 10, 11, 12, time.Local)
	if err := os.Chtimes(p, atime, mtime); err != nil {
		t.Fatal(err)
	}

	// cause mtime to be set to now
	if err := unix.Utimes(p, nil); err != nil {
		t.Fatalf("touch failed: %v", err)
	}
	now := time.Now()

	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if g, o := fi.ModTime(), mtime; !g.After(o) {
		t.Errorf("mtime did not progress: %v <= %v", g, o)
	}
	if g, e := fi.ModTime(), now; !timeEqualFuzzy(g, e, 1*time.Second) {
		t.Errorf("mtime is wrong: %v !~= %v", g, e)
	}
}

func TestFsync(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	p := path.Join(mnt.Dir, PrivateName, "jdoe", "myfile")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const input = "hello, world\n"
	if _, err := io.WriteString(f, input); err != nil {
		t.Fatalf("write error: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("fsync error: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close error: %v", err)
	}
}

func TestReaddirMyPublic(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	files := map[string]fileInfoCheck{
		"one": nil,
		"two": nil,
	}
	for filename := range files {
		if err := ioutil.WriteFile(path.Join(mnt.Dir, PublicName, "jdoe", filename), []byte("data for "+filename), 0644); err != nil {
			t.Fatal(err)
		}
	}

	checkDir(t, path.Join(mnt.Dir, PublicName, "jdoe"), files)
}

func TestReaddirOtherFolderAsReader(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()

		// cause the folder to exist
		if err := ioutil.WriteFile(path.Join(mnt.Dir, PrivateName, "jdoe#wsmith", "myfile"), []byte("data for myfile"), 0644); err != nil {
			t.Fatal(err)
		}
	}()

	c2 := libkbfs.ConfigAsUser(config, "wsmith")
	defer c2.Shutdown()
	mnt, _, cancelFn := makeFS(t, c2)
	defer mnt.Close()
	defer cancelFn()

	checkDir(t, path.Join(mnt.Dir, PrivateName, "jdoe#wsmith"), map[string]fileInfoCheck{
		"myfile": nil,
	})
}

func TestStatOtherFolder(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()

		// cause the folder to exist
		if err := ioutil.WriteFile(path.Join(mnt.Dir, PrivateName, "jdoe", "myfile"), []byte("data for myfile"), 0644); err != nil {
			t.Fatal(err)
		}
	}()

	c2 := libkbfs.ConfigAsUser(config, "wsmith")
	defer c2.Shutdown()
	mnt, _, cancelFn := makeFS(t, c2)
	defer mnt.Close()
	defer cancelFn()

	switch _, err := os.Lstat(path.Join(mnt.Dir, PrivateName, "jdoe")); err := err.(type) {
	case *os.PathError:
		if g, e := err.Err, syscall.EACCES; g != e {
			t.Fatalf("wrong error: %v != %v", g, e)
		}
	default:
		t.Fatalf("expected a PathError, got %T: %v", err, err)
	}
}

func TestStatOtherFolderFirstUse(t *testing.T) {
	// This triggers a different error than with the warmup.
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()

	c2 := libkbfs.ConfigAsUser(config, "wsmith")
	defer c2.Shutdown()
	mnt, _, cancelFn := makeFS(t, c2)
	defer mnt.Close()
	defer cancelFn()

	switch _, err := os.Lstat(path.Join(mnt.Dir, PrivateName, "jdoe")); err := err.(type) {
	case *os.PathError:
		if g, e := err.Err, syscall.EACCES; g != e {
			t.Fatalf("wrong error: %v != %v", g, e)
		}
	default:
		t.Fatalf("expected a PathError, got %T: %v", err, err)
	}
}

func TestStatOtherFolderPublic(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()

		// cause the folder to exist
		if err := ioutil.WriteFile(path.Join(mnt.Dir, PublicName, "jdoe", "myfile"), []byte("data for myfile"), 0644); err != nil {
			t.Fatal(err)
		}
	}()

	c2 := libkbfs.ConfigAsUser(config, "wsmith")
	defer c2.Shutdown()
	mnt, _, cancelFn := makeFS(t, c2)
	defer mnt.Close()
	defer cancelFn()

	fi, err := os.Lstat(path.Join(mnt.Dir, PublicName, "jdoe"))
	if err != nil {
		t.Fatal(err)
	}
	// TODO figure out right modes, note owner is the person running
	// fuse, not the person owning the folder
	if g, e := fi.Mode().String(), `drwxr-xr-x`; g != e {
		t.Errorf("wrong mode for folder: %q != %q", g, e)
	}
}

func TestStatOtherFolderPublicFirstUse(t *testing.T) {
	// This triggers a different error than with the warmup.
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()

	c2 := libkbfs.ConfigAsUser(config, "wsmith")
	defer c2.Shutdown()
	mnt, _, cancelFn := makeFS(t, c2)
	defer mnt.Close()
	defer cancelFn()

	switch _, err := os.Lstat(path.Join(mnt.Dir, PublicName, "jdoe")); err := err.(type) {
	case *os.PathError:
		if g, e := err.Err, syscall.EACCES; g != e {
			t.Fatalf("wrong error: %v != %v", g, e)
		}
	default:
		t.Fatalf("expected a PathError, got %T: %v", err, err)
	}
}

func TestReadPublicFile(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	const input = "hello, world\n"
	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()

		// cause the folder to exist
		if err := ioutil.WriteFile(path.Join(mnt.Dir, PublicName, "jdoe", "myfile"), []byte(input), 0644); err != nil {
			t.Fatal(err)
		}
	}()

	c2 := libkbfs.ConfigAsUser(config, "wsmith")
	defer c2.Shutdown()
	mnt, _, cancelFn := makeFS(t, c2)
	defer mnt.Close()
	defer cancelFn()

	buf, err := ioutil.ReadFile(path.Join(mnt.Dir, PublicName, "jdoe", "myfile"))
	if err != nil {
		t.Fatal(err)
	}
	if g, e := string(buf), input; g != e {
		t.Errorf("bad file contents: %q != %q", g, e)
	}
}

func TestReaddirOtherFolderPublicAsAnyone(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()

		// cause the folder to exist
		if err := ioutil.WriteFile(path.Join(mnt.Dir, PublicName, "jdoe", "myfile"), []byte("data for myfile"), 0644); err != nil {
			t.Fatal(err)
		}
	}()

	c2 := libkbfs.ConfigAsUser(config, "wsmith")
	defer c2.Shutdown()
	mnt, _, cancelFn := makeFS(t, c2)
	defer mnt.Close()
	defer cancelFn()

	checkDir(t, path.Join(mnt.Dir, PublicName, "jdoe"), map[string]fileInfoCheck{
		"myfile": nil,
	})
}

func TestReaddirOtherFolderAsAnyone(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	func() {
		mnt, _, cancelFn := makeFS(t, config)
		defer mnt.Close()
		defer cancelFn()

		// cause the folder to exist
		if err := ioutil.WriteFile(path.Join(mnt.Dir, PrivateName, "jdoe", "myfile"), []byte("data for myfile"), 0644); err != nil {
			t.Fatal(err)
		}
	}()

	c2 := libkbfs.ConfigAsUser(config, "wsmith")
	defer c2.Shutdown()
	mnt, _, cancelFn := makeFS(t, c2)
	defer mnt.Close()
	defer cancelFn()

	switch _, err := ioutil.ReadDir(path.Join(mnt.Dir, PrivateName, "jdoe")); err := err.(type) {
	case *os.PathError:
		if g, e := err.Err, syscall.EACCES; g != e {
			t.Fatalf("wrong error: %v != %v", g, e)
		}
	default:
		t.Fatalf("expected a PathError, got %T: %v", err, err)
	}
}

func syncFolderToServer(t *testing.T, tlf string, fs *FS) {
	dh, err := libkbfs.ParseTlfHandle(context.Background(), fs.config, tlf)
	if err != nil {
		t.Fatalf("cannot parse %s as folder: %v", tlf, err)
	}

	ctx := context.Background()
	root, _, err := fs.config.KBFSOps().GetOrCreateRootNodeForHandle(
		ctx, dh, libkbfs.MasterBranch)
	if err != nil {
		t.Fatalf("cannot get root for %s: %v", tlf, err)
	}

	err = fs.config.KBFSOps().SyncFromServer(ctx, root.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't sync from server: %v", err)
	}
	fs.notificationGroup.Wait()
}

func syncPublicFolderToServer(t *testing.T, tlf string, fs *FS) {
	syncFolderToServer(t, tlf+libkbfs.ReaderSep+libkbfs.PublicUIDName, fs)
}

func TestInvalidateDataOnWrite(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	mnt1, _, cancelFn1 := makeFS(t, config)
	defer mnt1.Close()
	defer cancelFn1()
	mnt2, fs2, cancelFn2 := makeFS(t, config)
	defer mnt2.Close()
	defer cancelFn2()

	if !mnt2.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	const input1 = "input round one"
	if err := ioutil.WriteFile(path.Join(mnt1.Dir, PrivateName, "jdoe", "myfile"), []byte(input1), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path.Join(mnt2.Dir, PrivateName, "jdoe", "myfile"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	{
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if g, e := string(buf[:n]), input1; g != e {
			t.Errorf("wrong content: %q != %q", g, e)
		}
	}

	const input2 = "second round of content"
	if err := ioutil.WriteFile(path.Join(mnt1.Dir, PrivateName, "jdoe", "myfile"), []byte(input2), 0644); err != nil {
		t.Fatal(err)
	}

	syncFolderToServer(t, "jdoe", fs2)

	{
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if g, e := string(buf[:n]), input2; g != e {
			t.Errorf("wrong content: %q != %q", g, e)
		}
	}
}

func TestInvalidatePublicDataOnWrite(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	mnt1, _, cancelFn1 := makeFS(t, config)
	defer mnt1.Close()
	defer cancelFn1()
	mnt2, fs2, cancelFn2 := makeFS(t, config)
	defer mnt2.Close()
	defer cancelFn2()

	if !mnt2.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	const input1 = "input round one"
	if err := ioutil.WriteFile(path.Join(mnt1.Dir, PublicName, "jdoe", "myfile"), []byte(input1), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path.Join(mnt2.Dir, PublicName, "jdoe", "myfile"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	{
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if g, e := string(buf[:n]), input1; g != e {
			t.Errorf("wrong content: %q != %q", g, e)
		}
	}

	const input2 = "second round of content"
	if err := ioutil.WriteFile(path.Join(mnt1.Dir, PublicName, "jdoe", "myfile"), []byte(input2), 0644); err != nil {
		t.Fatal(err)
	}

	syncPublicFolderToServer(t, "jdoe", fs2)

	{
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if g, e := string(buf[:n]), input2; g != e {
			t.Errorf("wrong content: %q != %q", g, e)
		}
	}
}

func TestInvalidateDataOnTruncate(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	mnt1, _, cancelFn1 := makeFS(t, config)
	defer mnt1.Close()
	defer cancelFn1()
	mnt2, fs2, cancelFn2 := makeFS(t, config)
	defer mnt2.Close()
	defer cancelFn2()

	if !mnt2.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	const input1 = "input round one"
	if err := ioutil.WriteFile(path.Join(mnt1.Dir, PrivateName, "jdoe", "myfile"), []byte(input1), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path.Join(mnt2.Dir, PrivateName, "jdoe", "myfile"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	{
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if g, e := string(buf[:n]), input1; g != e {
			t.Errorf("wrong content: %q != %q", g, e)
		}
	}

	const newSize = 3
	if err := os.Truncate(path.Join(mnt1.Dir, PrivateName, "jdoe", "myfile"), newSize); err != nil {
		t.Fatal(err)
	}

	syncFolderToServer(t, "jdoe", fs2)

	{
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if g, e := string(buf[:n]), input1[:newSize]; g != e {
			t.Errorf("wrong content: %q != %q", g, e)
		}
	}
}

func TestInvalidateDataOnLocalWrite(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	mnt, fs, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	if !mnt.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	const input1 = "input round one"
	if err := ioutil.WriteFile(path.Join(mnt.Dir, PrivateName, "jdoe", "myfile"), []byte(input1), 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path.Join(mnt.Dir, PrivateName, "jdoe", "myfile"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	{
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if g, e := string(buf[:n]), input1; g != e {
			t.Errorf("wrong content: %q != %q", g, e)
		}
	}

	const input2 = "second round of content"
	{
		ctx := context.Background()
		dh, err := libkbfs.ParseTlfHandle(ctx, config, "jdoe")
		if err != nil {
			t.Fatalf("cannot parse folder for jdoe: %v", err)
		}
		ops := config.KBFSOps()
		jdoe, _, err := ops.GetOrCreateRootNodeForHandle(ctx, dh, libkbfs.MasterBranch)
		if err != nil {
			t.Fatal(err)
		}
		myfile, _, err := ops.Lookup(ctx, jdoe, "myfile")
		if err != nil {
			t.Fatal(err)
		}
		if err := ops.Write(ctx, myfile, []byte(input2), 0); err != nil {
			t.Fatal(err)
		}
	}

	// The Write above is a local change, and thus we can just do a
	// local wait without syncing to the server.
	fs.notificationGroup.Wait()

	{
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if g, e := string(buf[:n]), input2; g != e {
			t.Errorf("wrong content: %q != %q", g, e)
		}
	}
}

func TestInvalidateEntryOnDelete(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe", "wsmith")
	defer config.Shutdown()
	mnt1, _, cancelFn1 := makeFS(t, config)
	defer mnt1.Close()
	defer cancelFn1()
	mnt2, fs2, cancelFn2 := makeFS(t, config)
	defer mnt2.Close()
	defer cancelFn2()

	if !mnt2.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	const input1 = "input round one"
	if err := ioutil.WriteFile(path.Join(mnt1.Dir, PrivateName, "jdoe", "myfile"), []byte(input1), 0644); err != nil {
		t.Fatal(err)
	}

	buf, err := ioutil.ReadFile(path.Join(mnt2.Dir, PrivateName, "jdoe", "myfile"))
	if err != nil {
		t.Fatal(err)
	}
	if g, e := string(buf), input1; g != e {
		t.Errorf("wrong content: %q != %q", g, e)
	}

	if err := os.Remove(path.Join(mnt1.Dir, PrivateName, "jdoe", "myfile")); err != nil {
		t.Fatal(err)
	}

	syncFolderToServer(t, "jdoe", fs2)

	if buf, err := ioutil.ReadFile(path.Join(mnt2.Dir, PrivateName, "jdoe", "myfile")); !os.IsNotExist(err) {
		t.Fatalf("expected ENOENT: %v: %q", err, buf)
	}
}

func testForErrorText(t *testing.T, path string, expectedErr error,
	fileType string) {
	buf, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatalf("Bad error reading %s error file: %v", err, fileType)
	}

	var errors []jsonReportedError
	err = json.Unmarshal(buf, &errors)
	if err != nil {
		t.Fatalf("Couldn't unmarshal error file: %v. Full contents: %s",
			err, string(buf))
	}

	found := false
	for _, e := range errors {
		if e.Error == expectedErr.Error() {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("%s error file did not contain the error %s. "+
			"Full contents: %s", fileType, expectedErr, buf)
	}
}

func TestErrorFile(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	config.SetReporter(libkbfs.NewReporterSimple(config.Clock(), 0))
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	// cause an error by stating a non-existent user
	_, err := os.Lstat(path.Join(mnt.Dir, PrivateName, "janedoe"))
	if err == nil {
		t.Fatal("Stat of non-existent user worked!")
	}

	// Make sure the root error file reads as expected
	expectedErr := libkbfs.NoSuchUserError{Input: "janedoe"}

	// test both the root error file and one in a directory
	testForErrorText(t, path.Join(mnt.Dir, libkbfs.ErrorFile),
		expectedErr, "root")
	testForErrorText(t, path.Join(mnt.Dir, PublicName, libkbfs.ErrorFile),
		expectedErr, "root")
	testForErrorText(t, path.Join(mnt.Dir, PrivateName, libkbfs.ErrorFile),
		expectedErr, "root")
	testForErrorText(t, path.Join(mnt.Dir, PublicName, "jdoe", libkbfs.ErrorFile),
		expectedErr, "dir")
	testForErrorText(t, path.Join(mnt.Dir, PrivateName, "jdoe", libkbfs.ErrorFile),
		expectedErr, "dir")
}

type testMountObserver struct {
	c chan<- struct{}
}

func (t *testMountObserver) LocalChange(ctx context.Context, node libkbfs.Node,
	write libkbfs.WriteRange) {
	// ignore
}

func (t *testMountObserver) BatchChanges(ctx context.Context,
	changes []libkbfs.NodeChange) {
	t.c <- struct{}{}
}

func TestInvalidateAcrossMounts(t *testing.T) {
	config1 := libkbfs.MakeTestConfigOrBust(t, "user1",
		"user2")
	mnt1, _, cancelFn1 := makeFS(t, config1)
	defer mnt1.Close()
	defer cancelFn1()

	config2 := libkbfs.ConfigAsUser(config1, "user2")
	mnt2, fs2, cancelFn2 := makeFS(t, config2)
	defer mnt2.Close()
	defer cancelFn2()

	if !mnt2.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	// user 1 writes one file to root and one to a sub directory
	const input1 = "input round one"
	myfile1 := path.Join(mnt1.Dir, PrivateName, "user1,user2", "myfile")
	if err := ioutil.WriteFile(myfile1, []byte(input1), 0644); err != nil {
		t.Fatal(err)
	}
	mydir1 := path.Join(mnt1.Dir, PrivateName, "user1,user2", "mydir")
	if err := os.Mkdir(mydir1, 0755); err != nil {
		t.Fatal(err)
	}
	mydira1 := path.Join(mydir1, "a")
	if err := ioutil.WriteFile(mydira1, []byte(input1), 0644); err != nil {
		t.Fatal(err)
	}

	myfile2 := path.Join(mnt2.Dir, PrivateName, "user1,user2", "myfile")
	buf, err := ioutil.ReadFile(myfile2)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := string(buf), input1; g != e {
		t.Errorf("wrong content: %q != %q", g, e)
	}

	mydir2 := path.Join(mnt2.Dir, PrivateName, "user1,user2", "mydir")
	mydira2 := path.Join(mydir2, "a")
	buf, err = ioutil.ReadFile(mydira2)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := string(buf), input1; g != e {
		t.Errorf("wrong content: %q != %q", g, e)
	}

	// now remove the first file, and rename the second
	if err := os.Remove(myfile1); err != nil {
		t.Fatal(err)
	}
	mydirb1 := path.Join(mydir1, "b")
	if err := os.Rename(mydira1, mydirb1); err != nil {
		t.Fatal(err)
	}

	syncFolderToServer(t, "user1,user2", fs2)

	// check everything from user 2's perspective
	if buf, err := ioutil.ReadFile(myfile2); !os.IsNotExist(err) {
		t.Fatalf("expected ENOENT: %v: %q", err, buf)
	}
	if buf, err := ioutil.ReadFile(mydira2); !os.IsNotExist(err) {
		t.Fatalf("expected ENOENT: %v: %q", err, buf)
	}

	checkDir(t, mydir2, map[string]fileInfoCheck{
		"b": func(fi os.FileInfo) error {
			if fi.Size() != int64(len(input1)) {
				return fmt.Errorf("Bad file size: %d", fi.Size())
			}
			return nil
		},
	})

	mydirb2 := path.Join(mydir2, "b")
	buf, err = ioutil.ReadFile(mydirb2)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := string(buf), input1; g != e {
		t.Errorf("wrong content: %q != %q", g, e)
	}
}

func TestInvalidateAppendAcrossMounts(t *testing.T) {
	config1 := libkbfs.MakeTestConfigOrBust(t, "user1",
		"user2")
	mnt1, _, cancelFn1 := makeFS(t, config1)
	defer mnt1.Close()
	defer cancelFn1()

	config2 := libkbfs.ConfigAsUser(config1, "user2")
	mnt2, fs2, cancelFn2 := makeFS(t, config2)
	defer mnt2.Close()
	defer cancelFn2()

	if !mnt2.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	// user 1 writes one file to root and one to a sub directory
	const input1 = "input round one"
	myfile1 := path.Join(mnt1.Dir, PrivateName, "user1,user2", "myfile")
	if err := ioutil.WriteFile(myfile1, []byte(input1), 0644); err != nil {
		t.Fatal(err)
	}
	myfile2 := path.Join(mnt2.Dir, PrivateName, "user1,user2", "myfile")
	buf, err := ioutil.ReadFile(myfile2)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := string(buf), input1; g != e {
		t.Errorf("wrong content: %q != %q", g, e)
	}

	// user 1 append using libkbfs, to ensure that it doesn't flush
	// the whole page.
	const input2 = "input round two"
	{
		ctx := context.Background()
		dh, err := libkbfs.ParseTlfHandle(ctx, config1, "user1,user2")
		if err != nil {
			t.Fatalf("cannot parse folder for jdoe: %v", err)
		}
		ops := config1.KBFSOps()
		jdoe, _, err := ops.GetOrCreateRootNodeForHandle(ctx, dh,
			libkbfs.MasterBranch)
		if err != nil {
			t.Fatal(err)
		}
		myfile, _, err := ops.Lookup(ctx, jdoe, "myfile")
		if err != nil {
			t.Fatal(err)
		}
		if err := ops.Write(
			ctx, myfile, []byte(input2), int64(len(input1))); err != nil {
			t.Fatal(err)
		}
		if err := ops.Sync(ctx, myfile); err != nil {
			t.Fatal(err)
		}
	}

	syncFolderToServer(t, "user1,user2", fs2)

	// check everything from user 2's perspective
	buf, err = ioutil.ReadFile(myfile2)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := string(buf), input1+input2; g != e {
		t.Errorf("wrong content: %q != %q", g, e)
	}
}

func TestInvalidateRenameToUncachedDir(t *testing.T) {
	config1 := libkbfs.MakeTestConfigOrBust(t, "user1",
		"user2")
	mnt1, fs1, cancelFn1 := makeFS(t, config1)
	defer mnt1.Close()
	defer cancelFn1()

	config2 := libkbfs.ConfigAsUser(config1, "user2")
	mnt2, fs2, cancelFn2 := makeFS(t, config2)
	defer mnt2.Close()
	defer cancelFn2()

	if !mnt2.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	// user 1 writes one file to root and one to a sub directory
	const input1 = "input round one"
	myfile1 := path.Join(mnt1.Dir, PrivateName, "user1,user2", "myfile")
	if err := ioutil.WriteFile(myfile1, []byte(input1), 0644); err != nil {
		t.Fatal(err)
	}
	mydir1 := path.Join(mnt1.Dir, PrivateName, "user1,user2", "mydir")
	if err := os.Mkdir(mydir1, 0755); err != nil {
		t.Fatal(err)
	}
	mydirfile1 := path.Join(mydir1, "myfile")

	myfile2 := path.Join(mnt2.Dir, PrivateName, "user1,user2", "myfile")
	f, err := os.OpenFile(myfile2, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	{
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if g, e := string(buf[:n]), input1; g != e {
			t.Errorf("wrong content: %q != %q", g, e)
		}
	}

	// now rename the second into a directory that user 2 hasn't seen
	if err := os.Rename(myfile1, mydirfile1); err != nil {
		t.Fatal(err)
	}

	syncFolderToServer(t, "user1,user2", fs2)

	// user 2 should be able to write to its open file, and user 1
	// will see the change
	const input2 = "input round two"
	{
		n, err := f.WriteAt([]byte(input2), 0)
		if err != nil || n != len(input2) {
			t.Fatal(err)
		}
	}
	f.Close()

	syncFolderToServer(t, "user1,user2", fs1)

	buf, err := ioutil.ReadFile(mydirfile1)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := string(buf), input2; g != e {
		t.Errorf("wrong content: %q != %q", g, e)
	}
}

func TestStatusFile(t *testing.T) {
	config := libkbfs.MakeTestConfigOrBust(t, "jdoe")
	defer config.Shutdown()
	mnt, _, cancelFn := makeFS(t, config)
	defer mnt.Close()
	defer cancelFn()

	ctx := context.Background()
	dh, err := libkbfs.ParseTlfHandle(ctx, config, "jdoe")
	ops := config.KBFSOps()
	jdoe, _, err := ops.GetOrCreateRootNodeForHandle(ctx, dh,
		libkbfs.MasterBranch)
	status, _, err := ops.Status(ctx, jdoe.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't get KBFS status: %v", err)
	}

	// Simply make sure the status in the file matches what we'd
	// expect.  Checking the exact content should be left for tests
	// within libkbfs.
	buf, err := ioutil.ReadFile(path.Join(mnt.Dir, PublicName, "jdoe",
		StatusFileName))
	if err != nil {
		t.Fatalf("Couldn't read KBFS status file: %v", err)
	}

	var bufStatus libkbfs.FolderBranchStatus
	json.Unmarshal(buf, &bufStatus)

	// It's safe to compare the path slices with DeepEqual since they
	// will all be null for this test (nothing is dirtied).
	if !reflect.DeepEqual(status, bufStatus) {
		t.Fatalf("Status file contents (%s) didn't match expected status %v",
			buf, status)
	}
}

// TODO: remove once we have automatic conflict resolution tests
func TestUnstageFile(t *testing.T) {
	config1 := libkbfs.MakeTestConfigOrBust(t, "user1",
		"user2")
	mnt1, _, cancelFn1 := makeFS(t, config1)
	defer mnt1.Close()
	defer cancelFn1()

	config2 := libkbfs.ConfigAsUser(config1, "user2")
	mnt2, fs2, cancelFn2 := makeFS(t, config2)
	defer mnt2.Close()
	defer cancelFn2()

	if !mnt2.Conn.Protocol().HasInvalidate() {
		t.Skip("Old FUSE protocol")
	}

	ctx := context.Background()

	// both users read the root dir first
	myroot1 := path.Join(mnt1.Dir, PrivateName, "user1,user2")
	myroot2 := path.Join(mnt2.Dir, PrivateName, "user1,user2")
	checkDir(t, myroot1, map[string]fileInfoCheck{})
	checkDir(t, myroot2, map[string]fileInfoCheck{})

	// turn updates off for user 2
	dh, err := libkbfs.ParseTlfHandle(ctx, config2, "user1,user2")
	rootNode2, _, err := config2.KBFSOps().GetOrCreateRootNodeForHandle(ctx, dh,
		libkbfs.MasterBranch)
	_, err = libkbfs.DisableUpdatesForTesting(config2,
		rootNode2.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't pause user 2 updates")
	}

	// user1 writes a file and makes a few directories
	const input1 = "input round one"
	myfile1 := path.Join(mnt1.Dir, PrivateName, "user1,user2", "myfile")
	if err := ioutil.WriteFile(myfile1, []byte(input1), 0644); err != nil {
		t.Fatal(err)
	}
	mydir1 := path.Join(mnt1.Dir, PrivateName, "user1,user2", "mydir")
	if err := os.Mkdir(mydir1, 0755); err != nil {
		t.Fatal(err)
	}
	mysubdir1 := path.Join(mnt1.Dir, PrivateName, "user1,user2", "mydir",
		"mysubdir")
	if err := os.Mkdir(mysubdir1, 0755); err != nil {
		t.Fatal(err)
	}

	// user2 does similar
	const input2 = "input round two"
	myfile2 := path.Join(mnt2.Dir, PrivateName, "user1,user2", "myfile")
	if err := ioutil.WriteFile(myfile2, []byte(input2), 0644); err != nil {
		t.Fatal(err)
	}
	mydir2 := path.Join(mnt2.Dir, PrivateName, "user1,user2", "mydir")
	if err := os.Mkdir(mydir2, 0755); err != nil {
		t.Fatal(err)
	}
	myothersubdir2 := path.Join(mnt2.Dir, PrivateName, "user1,user2", "mydir",
		"myothersubdir")
	if err := os.Mkdir(myothersubdir2, 0755); err != nil {
		t.Fatal(err)
	}

	// verify that they don't see each other's files
	checkDir(t, mydir1, map[string]fileInfoCheck{
		"mysubdir": mustBeDir,
	})
	checkDir(t, mydir2, map[string]fileInfoCheck{
		"myothersubdir": mustBeDir,
	})

	// now unstage user 2 and they should see the same stuff
	unstageFile2 := path.Join(mnt2.Dir, PrivateName, "user1,user2",
		UnstageFileName)
	if err := ioutil.WriteFile(unstageFile2, []byte{1}, 0222); err != nil {
		t.Fatal(err)
	}

	syncFolderToServer(t, "user1,user2", fs2)

	// They should see identical folders now
	checkDir(t, mydir1, map[string]fileInfoCheck{
		"mysubdir": mustBeDir,
	})
	checkDir(t, mydir2, map[string]fileInfoCheck{
		"mysubdir": mustBeDir,
	})

	buf, err := ioutil.ReadFile(myfile1)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := string(buf), input1; g != e {
		t.Errorf("wrong content: %q != %q", g, e)
	}
	buf, err = ioutil.ReadFile(myfile2)
	if err != nil {
		t.Fatal(err)
	}
	if g, e := string(buf), input1; g != e {
		t.Errorf("wrong content: %q != %q", g, e)
	}
}
