package main

import (
	"flag"
	"fmt"
	"os"

	"golang.org/x/net/context"

	"github.com/keybase/client/go/logger"
	"github.com/keybase/kbfs/libkbfs"
)

var version = flag.Bool("version", false, "Print version")

const usageFormatStr = `Usage:
  kbfs -version

To run against remote KBFS servers:
  kbfs [-debug] [-cpuprofile=path/to/dir] [-bserver=%s] [-mdserver=%s]
    <command> [<args>]

To run in a local testing environment:
  kbfs [-debug] [-cpuprofile=path/to/dir]
    [-server-in-memory|-server-root=path/to/dir] [-localuser=<user>]
    <command> [<args>]

The possible commands are:
  stat		Display file status
  ls		List directory contents
  mkdir		Make directories
  read		Dump file to stdout
  write		Write stdin to file

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

// Define this so deferred functions get executed before exit.
func realMain() (exitStatus int) {
	kbfsParams := libkbfs.AddFlags(flag.CommandLine)

	flag.Parse()

	if *version {
		fmt.Printf("%s\n", libkbfs.VersionString())
		return 0
	}

	if len(flag.Args()) < 1 {
		fmt.Print(getUsageStr())
		return 1
	}

	log := logger.NewWithCallDepth("", 1, os.Stderr)

	config, err := libkbfs.Init(*kbfsParams, nil, log)
	if err != nil {
		printError("kbfs", err)
		return 1
	}

	defer libkbfs.Shutdown()

	// TODO: Make the logging level WARNING instead of INFO, or
	// figure out some other way to log the full folder-branch
	// name for kbfsfuse but not for kbfs.

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
		return 1
	}
}

func main() {
	os.Exit(realMain())
}
