// Keybase file system

package main

import (
	"flag"
	"fmt"
	"os"

	"bazil.org/fuse"

	"github.com/keybase/client/go/logger"
	"github.com/keybase/kbfs/libfs"
	"github.com/keybase/kbfs/libfuse"
	"github.com/keybase/kbfs/libkbfs"
)

var runtimeDir = flag.String("runtime-dir", os.Getenv("KEYBASE_RUNTIME_DIR"), "runtime directory")
var label = flag.String("label", os.Getenv("KEYBASE_LABEL"), "label to help identify if running as a service")
var mountType = flag.String("mount-type", defaultMountType, "mount type: default, force")
var version = flag.Bool("version", false, "Print version")

const usageFormatStr = `Usage:
  kbfsfuse -version

To run against remote KBFS servers:
  kbfsfuse [-debug] [-cpuprofile=path/to/dir]
    [-bserver=%s] [-mdserver=%s]
    [-runtime-dir=path/to/dir] [-label=label] [-mount-type=force]
    /path/to/mountpoint

To run in a local testing environment:
  kbfsfuse [-debug] [-cpuprofile=path/to/dir]
    [-server-in-memory|-server-root=path/to/dir] [-localuser=<user>]
    [-runtime-dir=path/to/dir] [-label=label] [-mount-type=force]
    /path/to/mountpoint

`

func getUsageStr() string {
	defaultBServer := libkbfs.GetDefaultBServer()
	if len(defaultBServer) == 0 {
		defaultBServer = "host:port"
	}
	defaultMDServer := libkbfs.GetDefaultMDServer()
	if len(defaultMDServer) == 0 {
		defaultMDServer = "host:port"
	}
	return fmt.Sprintf(usageFormatStr, defaultBServer, defaultMDServer)
}

func start() *libfs.Error {
	kbfsParams := libkbfs.AddFlags(flag.CommandLine)

	flag.Parse()

	if *version {
		fmt.Printf("%s\n", libkbfs.VersionString())
		return nil
	}

	if len(flag.Args()) < 1 {
		fmt.Print(getUsageStr())
		return libfs.InitError("no mount specified")
	}

	if len(flag.Args()) > 1 {
		fmt.Print(getUsageStr())
		return libfs.InitError("extra arguments specified (flags go before the first argument)")
	}

	if kbfsParams.Debug {
		fuseLog := logger.NewWithCallDepth("FUSE", 1, os.Stderr)
		fuseLog.Configure("", true, "")
		fuse.Debug = func(msg interface{}) {
			fuseLog.Debug("%s", msg)
		}
	}

	mountpoint := flag.Arg(0)
	var mounter libfuse.Mounter
	if *mountType == "force" {
		mounter = libfuse.NewForceMounter(mountpoint)
	} else {
		mounter = libfuse.NewDefaultMounter(mountpoint)
	}

	options := libfuse.StartOptions{
		KbfsParams: *kbfsParams,
		RuntimeDir: *runtimeDir,
		Label:      *label,
	}

	return libfuse.Start(mounter, options)
}

func main() {
	err := start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kbfsfuse error: (%d) %s\n", err.Code, err.Message)

		os.Exit(err.Code)
	}
	os.Exit(0)
}
