package libfuse

import (
	"os"
	"strings"
	"sync"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/keybase/kbfs/libkbfs"
	"golang.org/x/net/context"
)

// FolderList is a node that can list all of the logged-in user's
// favorite top-level folders, on either a public or private basis.
type FolderList struct {
	fs *FS
	// only accept public folders
	public bool

	mu      sync.Mutex
	folders map[string]*TLF
}

var _ fs.Node = (*FolderList)(nil)

// Attr implements the fs.Node interface.
func (*FolderList) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0755
	return nil
}

var _ fs.NodeRequestLookuper = (*FolderList)(nil)

// Lookup implements the fs.NodeRequestLookuper interface.
func (fl *FolderList) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (node fs.Node, err error) {
	fl.fs.log.CDebugf(ctx, "FL Lookup %s", req.Name)
	defer func() { fl.fs.reportErr(ctx, err) }()
	fl.mu.Lock()
	defer fl.mu.Unlock()

	specialNode := handleSpecialFile(req.Name, fl.fs, resp)
	if specialNode != nil {
		return specialNode, nil
	}

	if child, ok := fl.folders[req.Name]; ok {
		return child, nil
	}

	// Shortcut for dreaded extraneous OSX finder lookups
	if strings.HasPrefix(req.Name, "._") {
		return nil, fuse.ENOENT
	}

	_, err = libkbfs.ParseTlfHandle(
		ctx, fl.fs.config.KBPKI(), req.Name, fl.public)
	switch err := err.(type) {
	case nil:
		// No error.
		break

	case libkbfs.TlfNameNotCanonical:
		// Non-canonical name.
		n := &Alias{
			canon: err.NameToTry,
		}
		return n, nil

	case libkbfs.NoSuchNameError:
		// Invalid public TLF.
		return nil, fuse.ENOENT

	default:
		// Some other error.
		return nil, err
	}

	child := newTLF(fl, req.Name)
	fl.folders[req.Name] = child
	return child, nil
}

func (fl *FolderList) forgetFolder(folderName string) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	delete(fl.folders, folderName)
}

var _ fs.Handle = (*FolderList)(nil)

var _ fs.HandleReadDirAller = (*FolderList)(nil)

// ReadDirAll implements the ReadDirAll interface.
func (fl *FolderList) ReadDirAll(ctx context.Context) (res []fuse.Dirent, err error) {
	fl.fs.log.CDebugf(ctx, "FL ReadDirAll")
	defer func() {
		fl.fs.reportErr(ctx, err)
	}()
	_, _, err = fl.fs.config.KBPKI().GetCurrentUserInfo(ctx)
	isLoggedIn := err == nil

	var favs []*libkbfs.Favorite
	if isLoggedIn {
		favs, err = fl.fs.config.KBFSOps().GetFavorites(ctx)
		if err != nil {
			return nil, err
		}
	}

	res = make([]fuse.Dirent, 0, len(favs))
	for _, fav := range favs {
		if fav.Public != fl.public {
			continue
		}
		res = append(res, fuse.Dirent{
			Type: fuse.DT_Dir,
			Name: fav.Name,
		})
	}
	return res, nil
}

var _ fs.NodeRemover = (*FolderList)(nil)

// Remove implements the fs.NodeRemover interface for FolderList.
func (fl *FolderList) Remove(ctx context.Context, req *fuse.RemoveRequest) (err error) {
	return fuse.EPERM
}
