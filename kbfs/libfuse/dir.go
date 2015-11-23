package libfuse

import (
	"fmt"
	"math"
	"os"
	"sync"
	"syscall"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/keybase/kbfs/libkbfs"
	"golang.org/x/net/context"
)

// Folder represents KBFS top-level folders
type Folder struct {
	fs           *FS
	list         *FolderList
	name         string
	folderBranch libkbfs.FolderBranch

	// Protects the nodes map.
	mu sync.Mutex
	// Map KBFS nodes to FUSE nodes, to be able to handle multiple
	// lookups and incoming change notifications. A node is present
	// here if the kernel holds a reference to it.
	//
	// If we ever support hardlinks, this would need refcounts.
	//
	// Children must call folder.forgetChildLocked on receiving the
	// FUSE Forget request.
	nodes map[libkbfs.NodeID]fs.Node

	// Protects the updateChan.
	updateMu sync.Mutex
	// updateChan is non-nil when the user disables updates via the
	// file system.  Sending a struct{}{} on this channel will unpause
	// the updates.
	updateChan chan<- struct{}
}

// forgetNode forgets a formerly active child with basename name.
func (f *Folder) forgetNode(node libkbfs.Node) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.nodes, node.GetID())
	if len(f.nodes) == 0 {
		f.list.forgetFolder(f)
	}
}

var _ libkbfs.Observer = (*Folder)(nil)

// invalidateNodeDataRange notifies the kernel to invalidate cached data for node.
//
// The arguments follow KBFS semantics:
//
//     - Len > 0: "bytes Off..Off+Len were mutated"
//     - Len == 0: "new file Len is Off"
//
// For comparison, the FUSE semantics are:
//
//     - Len < 0: "forget data in range Off..infinity"
//     - Len > 0: "forget data in range Off..Off+Len"
func (f *Folder) invalidateNodeDataRange(node fs.Node, write libkbfs.WriteRange) error {
	off := int64(write.Off)
	size := int64(write.Len)
	if write.Off > math.MaxInt64 || write.Len > math.MaxInt64 {
		// out of bounds, just invalidate all data
		off = 0
		size = -1
	}
	if write.Len == 0 {
		// truncate, invalidate all data in the now-lost tail
		size = -1
	}

	// XXX: Work around https://github.com/keybase/kbfs/issues/417 by
	// just invalidating all data every time.
	off = 0
	size = -1

	// Off=0 Len=0 is the same as calling InvalidateNodeDataAttr; we
	// can just let that go through InvalidateNodeDataRange.
	if err := f.fs.fuse.InvalidateNodeDataRange(node, off, size); err != nil {
		return err
	}
	return nil
}

// LocalChange is called for changes originating within in this process.
func (f *Folder) LocalChange(ctx context.Context, node libkbfs.Node, write libkbfs.WriteRange) {
	if !f.fs.conn.Protocol().HasInvalidate() {
		// OSXFUSE 2.x does not support notifications
		return
	}
	if origin, ok := ctx.Value(CtxAppIDKey).(*FS); ok && origin == f.fs {
		return
	}

	// spawn a goroutine because we shouldn't lock during the notification
	f.fs.queueNotification(func() { f.localChangeInvalidate(ctx, node, write) })
}

func (f *Folder) localChangeInvalidate(ctx context.Context, node libkbfs.Node,
	write libkbfs.WriteRange) {
	f.mu.Lock()
	n, ok := f.nodes[node.GetID()]
	f.mu.Unlock()
	if !ok {
		return
	}

	if err := f.invalidateNodeDataRange(n, write); err != nil && err != fuse.ErrNotCached {
		// TODO we have no mechanism to do anything about this
		f.fs.log.CErrorf(ctx, "FUSE invalidate error: %v", err)
	}
}

// BatchChanges is called for changes originating anywhere, including
// other hosts.
func (f *Folder) BatchChanges(ctx context.Context, changes []libkbfs.NodeChange) {
	if !f.fs.conn.Protocol().HasInvalidate() {
		// OSXFUSE 2.x does not support notifications
		return
	}
	if origin, ok := ctx.Value(CtxAppIDKey).(*FS); ok && origin == f.fs {
		return
	}

	// spawn a goroutine because we shouldn't lock during the notification
	f.fs.queueNotification(func() { f.batchChangesInvalidate(ctx, changes) })
}

func (f *Folder) batchChangesInvalidate(ctx context.Context,
	changes []libkbfs.NodeChange) {
	for _, v := range changes {
		f.mu.Lock()
		n, ok := f.nodes[v.Node.GetID()]
		f.mu.Unlock()
		if !ok {
			continue
		}

		switch {
		case len(v.DirUpdated) > 0:
			// invalidate potentially cached Readdir contents
			if err := f.fs.fuse.InvalidateNodeData(n); err != nil && err != fuse.ErrNotCached {
				// TODO we have no mechanism to do anything about this
				f.fs.log.CErrorf(ctx, "FUSE invalidate error: %v", err)
			}
			for _, name := range v.DirUpdated {
				// invalidate the dentry cache
				if err := f.fs.fuse.InvalidateEntry(n, name); err != nil && err != fuse.ErrNotCached {
					// TODO we have no mechanism to do anything about this
					f.fs.log.CErrorf(ctx, "FUSE invalidate error: %v", err)
				}
			}

		case len(v.FileUpdated) > 0:
			for _, write := range v.FileUpdated {
				if err := f.invalidateNodeDataRange(n, write); err != nil && err != fuse.ErrNotCached {
					// TODO we have no mechanism to do anything about this
					f.fs.log.CErrorf(ctx, "FUSE invalidate error: %v", err)
				}
			}

		default:
			// just the attributes
			if err := f.fs.fuse.InvalidateNodeAttr(n); err != nil && err != fuse.ErrNotCached {
				// TODO we have no mechanism to do anything about this
				f.fs.log.CErrorf(ctx, "FUSE invalidate error: %v", err)
			}
		}
	}
}

// Dir represents KBFS subdirectories.
type Dir struct {
	folder *Folder
	node   libkbfs.Node
}

func newDir(folder *Folder, node libkbfs.Node) *Dir {
	d := &Dir{
		folder: folder,
		node:   node,
	}
	return d
}

var _ fs.Node = (*Dir)(nil)

// Attr implements the fs.Node interface for Dir.
func (d *Dir) Attr(ctx context.Context, a *fuse.Attr) (err error) {
	ctx = NewContextWithOpID(ctx, d.folder.fs.log)
	d.folder.fs.log.CDebugf(ctx, "Dir Attr")
	defer func() { d.folder.fs.reportErr(ctx, err) }()

	return d.attr(ctx, a)
}

func (d *Dir) attr(ctx context.Context, a *fuse.Attr) (err error) {
	de, err := d.folder.fs.config.KBFSOps().Stat(ctx, d.node)
	if err != nil {
		if _, ok := err.(libkbfs.NoSuchNameError); ok {
			return fuse.ESTALE
		}
		return err
	}
	fillAttr(&de, a)

	a.Mode = os.ModeDir | 0700
	if d.folder.folderBranch.Tlf.IsPublic() {
		a.Mode |= 0055
	}
	return nil
}

var _ fs.NodeRequestLookuper = (*Dir)(nil)

// Lookup implements the fs.NodeRequestLookuper interface for Dir.
func (d *Dir) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (node fs.Node, err error) {
	ctx = NewContextWithOpID(ctx, d.folder.fs.log)
	d.folder.fs.log.CDebugf(ctx, "Dir Lookup %s", req.Name)
	defer func() { d.folder.fs.reportErr(ctx, err) }()

	switch req.Name {
	case libkbfs.ErrorFile:
		return NewErrorFile(d.folder.fs, resp), nil

	case MetricsFileName:
		return NewMetricsFile(d.folder.fs, resp), nil

	case StatusFileName:
		return NewStatusFile(d.folder, resp), nil

	case UnstageFileName:
		resp.EntryValid = 0
		child := &UnstageFile{
			folder: d.folder,
		}
		return child, nil

	case DisableUpdatesFileName:
		resp.EntryValid = 0
		child := &UpdatesFile{
			folder: d.folder,
		}
		return child, nil

	case EnableUpdatesFileName:
		resp.EntryValid = 0
		child := &UpdatesFile{
			folder: d.folder,
			enable: true,
		}
		return child, nil

	case RekeyFileName:
		resp.EntryValid = 0
		child := &RekeyFile{
			folder: d.folder,
		}
		return child, nil
	}

	newNode, de, err := d.folder.fs.config.KBFSOps().Lookup(ctx, d.node, req.Name)
	if err != nil {
		if _, ok := err.(libkbfs.NoSuchNameError); ok {
			return nil, fuse.ENOENT
		}
		return nil, err
	}

	// No libkbfs calls after this point!
	d.folder.mu.Lock()
	defer d.folder.mu.Unlock()

	// newNode can be nil even without errors when the KBFS direntry
	// is of a type that doesn't get its own node (is fully contained
	// in the directory); Symlink does this.
	if newNode != nil {
		if n, ok := d.folder.nodes[newNode.GetID()]; ok {
			return n, nil
		}
	}

	switch de.Type {
	default:
		return nil, fmt.Errorf("unhandled entry type: %v", de.Type)

	case libkbfs.File, libkbfs.Exec:
		child := &File{
			folder: d.folder,
			node:   newNode,
		}
		d.folder.nodes[newNode.GetID()] = child
		return child, nil

	case libkbfs.Dir:
		child := newDir(d.folder, newNode)
		d.folder.nodes[newNode.GetID()] = child
		return child, nil

	case libkbfs.Sym:
		child := &Symlink{
			parent: d,
			name:   req.Name,
		}
		// a Symlink is never included in Folder.nodes, as it doesn't
		// have a libkbfs.Node to keep track of renames.
		return child, nil
	}
}

var _ fs.NodeCreater = (*Dir)(nil)

// Create implements the fs.NodeCreater interface for Dir.
func (d *Dir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (node fs.Node, handle fs.Handle, err error) {
	ctx = NewContextWithOpID(ctx, d.folder.fs.log)
	d.folder.fs.log.CDebugf(ctx, "Dir Create %s", req.Name)
	defer func() { d.folder.fs.reportErr(ctx, err) }()

	isExec := (req.Mode.Perm() & 0100) != 0
	newNode, _, err := d.folder.fs.config.KBFSOps().CreateFile(
		ctx, d.node, req.Name, isExec)
	if err != nil {
		return nil, nil, err
	}

	child := &File{
		folder: d.folder,
		node:   newNode,
	}
	d.folder.mu.Lock()
	d.folder.nodes[newNode.GetID()] = child
	d.folder.mu.Unlock()
	return child, child, nil
}

var _ fs.NodeMkdirer = (*Dir)(nil)

// Mkdir implements the fs.NodeMkdirer interface for Dir.
func (d *Dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (
	node fs.Node, err error) {
	ctx = NewContextWithOpID(ctx, d.folder.fs.log)
	d.folder.fs.log.CDebugf(ctx, "Dir Mkdir %s", req.Name)
	defer func() { d.folder.fs.reportErr(ctx, err) }()

	newNode, _, err := d.folder.fs.config.KBFSOps().CreateDir(
		ctx, d.node, req.Name)
	if err != nil {
		return nil, err
	}

	child := newDir(d.folder, newNode)
	d.folder.mu.Lock()
	d.folder.nodes[newNode.GetID()] = child
	d.folder.mu.Unlock()
	return child, nil
}

var _ fs.NodeSymlinker = (*Dir)(nil)

// Symlink implements the fs.NodeSymlinker interface for Dir.
func (d *Dir) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (
	node fs.Node, err error) {
	ctx = NewContextWithOpID(ctx, d.folder.fs.log)
	d.folder.fs.log.CDebugf(ctx, "Dir Symlink %s -> %s",
		req.NewName, req.Target)
	defer func() { d.folder.fs.reportErr(ctx, err) }()

	if _, err := d.folder.fs.config.KBFSOps().CreateLink(
		ctx, d.node, req.NewName, req.Target); err != nil {
		return nil, err
	}

	child := &Symlink{
		parent: d,
		name:   req.NewName,
	}
	return child, nil
}

var _ fs.NodeRenamer = (*Dir)(nil)

// Rename implements the fs.NodeRenamer interface for Dir.
func (d *Dir) Rename(ctx context.Context, req *fuse.RenameRequest,
	newDir fs.Node) (err error) {
	ctx = NewContextWithOpID(ctx, d.folder.fs.log)
	d.folder.fs.log.CDebugf(ctx, "Dir Rename %s -> %s",
		req.OldName, req.NewName)
	defer func() { d.folder.fs.reportErr(ctx, err) }()

	newDir2, ok := newDir.(*Dir)
	if !ok {
		// The destination is not a Dir instance, probably because
		// it's Root (or some other node type added later). The kernel
		// won't let a rename newDir point to a non-directory.
		//
		// We have no cheap atomic rename across folders, so we can't
		// serve this. EXDEV makes `mv` do a copy+delete, and the
		// Lookup on the destination path will decide whether it's
		// legal.
		return fuse.Errno(syscall.EXDEV)
	}
	if d.folder != newDir2.folder {
		// Check this explicitly, not just trusting KBFSOps.Rename to
		// return an error, because we rely on it for locking
		// correctness.
		return fuse.Errno(syscall.EXDEV)
	}

	// overwritten node, if any, will be removed from Folder.nodes, if
	// it is there in the first place, by its Forget

	if err := d.folder.fs.config.KBFSOps().Rename(
		ctx, d.node, req.OldName, newDir2.node, req.NewName); err != nil {
		return err
	}

	return nil
}

var _ fs.NodeRemover = (*Dir)(nil)

// Remove implements the fs.NodeRemover interface for Dir.
func (d *Dir) Remove(ctx context.Context, req *fuse.RemoveRequest) (err error) {
	ctx = NewContextWithOpID(ctx, d.folder.fs.log)
	d.folder.fs.log.CDebugf(ctx, "Dir Remove %s", req.Name)
	defer func() { d.folder.fs.reportErr(ctx, err) }()

	// node will be removed from Folder.nodes, if it is there in the
	// first place, by its Forget

	if req.Dir {
		err = d.folder.fs.config.KBFSOps().RemoveDir(ctx, d.node, req.Name)
	} else {
		err = d.folder.fs.config.KBFSOps().RemoveEntry(ctx, d.node, req.Name)
	}
	if err != nil {
		return err
	}

	return nil
}

var _ fs.Handle = (*Dir)(nil)

var _ fs.HandleReadDirAller = (*Dir)(nil)

// ReadDirAll implements the fs.NodeReadDirAller interface for Dir.
func (d *Dir) ReadDirAll(ctx context.Context) (res []fuse.Dirent, err error) {
	ctx = NewContextWithOpID(ctx, d.folder.fs.log)
	d.folder.fs.log.CDebugf(ctx, "Dir ReadDirAll")
	defer func() { d.folder.fs.reportErr(ctx, err) }()

	children, err := d.folder.fs.config.KBFSOps().GetDirChildren(ctx, d.node)
	if err != nil {
		return nil, err
	}

	for name, et := range children {
		fde := fuse.Dirent{
			Name: name,
		}
		switch et {
		case libkbfs.File, libkbfs.Exec:
			fde.Type = fuse.DT_File
		case libkbfs.Dir:
			fde.Type = fuse.DT_Dir
		case libkbfs.Sym:
			fde.Type = fuse.DT_Link
		}
		res = append(res, fde)
	}
	return res, nil
}

var _ fs.NodeForgetter = (*Dir)(nil)

// Forget kernel reference to this node.
func (d *Dir) Forget() {
	d.folder.forgetNode(d.node)
}

var _ fs.NodeSetattrer = (*Dir)(nil)

// Setattr implements the fs.NodeSetattrer interface for File.
func (d *Dir) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) (err error) {
	ctx = NewContextWithOpID(ctx, d.folder.fs.log)
	d.folder.fs.log.CDebugf(ctx, "Dir SetAttr")
	defer func() { d.folder.fs.reportErr(ctx, err) }()

	valid := req.Valid

	if valid.Mode() {
		// You can't set the mode on KBFS directories.
		return fuse.EPERM
	}

	if valid.Mtime() {
		err := d.folder.fs.config.KBFSOps().SetMtime(
			ctx, d.node, &req.Mtime)
		if err != nil {
			return err
		}
		valid &^= fuse.SetattrMtime | fuse.SetattrMtimeNow
	}

	// KBFS has no concept of persistent atime; explicitly don't handle it
	valid &^= fuse.SetattrAtime | fuse.SetattrAtimeNow

	// things we don't need to explicitly handle
	valid &^= fuse.SetattrLockOwner | fuse.SetattrHandle

	if valid != 0 {
		// don't let an unhandled operation slip by without error
		d.folder.fs.log.CInfof(ctx, "Setattr did not handle %v", valid)
		return fuse.ENOSYS
	}

	// Something in Linux kernel *requires* directories to provide
	// attributes here, where it was just an optimization for files.
	if err := d.attr(ctx, &resp.Attr); err != nil {
		return err
	}
	return nil
}
