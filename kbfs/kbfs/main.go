package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"golang.org/x/net/context"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/kbfs/libkbfs"
)

var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")
var memprofile = flag.String("memprofile", "", "write memory profile to file")
var local = flag.Bool("local", false,
	"use a fake local user DB instead of Keybase")
var localUserFlag = flag.String("localuser", "strib",
	"fake local user (only valid when local=true)")
var clientFlag = flag.Bool("client", false, "use keybase daemon")
var serverRootDirFlag = flag.String("server-root", "", "directory to put local server files (default is cwd)")
var serverInMemoryFlag = flag.Bool("server-in-memory", false, "use in-memory server (and ignore -server-root)")
var debug = flag.Bool("debug", false, "Print debug messages")

const usageStr = `Usage:
  kbfs [-client | -local [-localuser=<user>]]
    [-server-in-memory|-server-root=path/to/dir] <command> [<args>]

The possible commands are:
  stat		Display file status
  ls		List directory contents
  mkdir		Make directories
  read		Dump file to stdout
  write		Write stdin to file

`

// Define this so deferred functions get executed before exit.
func realMain() (exitStatus int) {
	flag.Parse()
	if len(flag.Args()) < 1 {
		fmt.Print(usageStr)
		exitStatus = 1
		return
	}

	var localUser libkb.NormalizedUsername
	if *local {
		localUser = libkb.NewNormalizedUsername(*localUserFlag)
	} else if *clientFlag {
		localUser = libkb.NormalizedUsername("")
	} else {
		printError("kbfs", errors.New("either -client or -local must be used"))
		exitStatus = 1
		return
	}

	var serverRootDir *string
	if !*serverInMemoryFlag {
		serverRootDir = serverRootDirFlag
	}

	config, err := libkbfs.Init(localUser, serverRootDir, *cpuprofile, *memprofile, nil, *debug)
	if err != nil {
		printError("kbfs", err)
		exitStatus = 1
		return
	}

	defer libkbfs.Shutdown(*memprofile)

	cmd := flag.Arg(0)
	args := flag.Args()[1:]

	ctx := context.Background()

	switch cmd {
	case "stat":
		return stat(ctx, config, args)
	case "ls":
		return ls(ctx, config, args)
	case "mkdir":
		return mkdir(ctx, config, args)
	case "read":
		return read(ctx, config, args)
	case "write":
		return write(ctx, config, args)
	default:
		printError("kbfs", fmt.Errorf("unknown command '%s'", cmd))
		exitStatus = 1
		return
	}
}

func main() {
	os.Exit(realMain())
}
