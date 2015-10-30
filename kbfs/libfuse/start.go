package libfuse

import (
	"os"
	"path"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/logger"
	"github.com/keybase/kbfs/libkbfs"
	"golang.org/x/net/context"
)

// StartOptions are options for starting up
type StartOptions struct {
	LocalUser     libkb.NormalizedUsername
	ServerRootDir *string
	CPUProfile    string
	MemProfile    string
	RuntimeDir    string
	Label         string
	Debug         bool
	BServerAddr   string
	MDServerAddr  string
}

// Start the filesystem
func Start(mounter Mounter, options StartOptions) *Error {
	c, err := mounter.Mount()
	if err != nil {
		return MountError(err.Error())
	}
	defer c.Close()

	onInterruptFn := func() {
		select {
		case <-c.Ready:
			// Was mounted, so try to unmount if it was successful.
			if c.MountError == nil {
				err = mounter.Unmount()
				if err != nil {
					return
				}
			}

		default:
			// Was not mounted successfully yet, so do nothing. Note that the mount
			// could still happen, but that's a rare enough edge case.
		}
	}

	config, err := libkbfs.Init(options.LocalUser, options.ServerRootDir,
		options.CPUProfile, options.MemProfile, onInterruptFn, options.Debug,
		options.BServerAddr, options.MDServerAddr)
	if err != nil {
		return InitError(err.Error())
	}

	defer libkbfs.Shutdown(options.MemProfile)

	if options.RuntimeDir != "" {
		info := libkb.NewServiceInfo(libkbfs.Version, libkbfs.Build, options.Label, os.Getpid())
		err = info.WriteFile(path.Join(options.RuntimeDir, "kbfs.info"))
		if err != nil {
			return InitError(err.Error())
		}
	}

	fs := NewFS(config, c, options.Debug)
	ctx := context.WithValue(context.Background(), CtxAppIDKey, &fs)
	logTags := make(logger.CtxLogTags)
	logTags[CtxIDKey] = CtxOpID
	ctx = logger.NewContextWithLogTags(ctx, logTags)
	fs.Serve(ctx)

	<-c.Ready
	err = c.MountError
	if err != nil {
		return MountError(err.Error())
	}

	return nil
}
