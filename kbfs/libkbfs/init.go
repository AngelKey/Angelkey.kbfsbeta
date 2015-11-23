package libkbfs

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strings"

	"github.com/keybase/client/go/client"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
)

func makeMDServer(config Config, serverRootDir *string, mdserverAddr string) (
	MDServer, error) {
	if serverRootDir == nil {
		// local in-memory MD server
		return NewMDServerMemory(config)
	}

	if len(mdserverAddr) == 0 {
		// local persistent MD server
		handlePath := filepath.Join(*serverRootDir, "kbfs_handles")
		mdPath := filepath.Join(*serverRootDir, "kbfs_md")
		branchPath := filepath.Join(*serverRootDir, "kbfs_branches")
		return NewMDServerLocal(
			config, handlePath, mdPath, branchPath)
	}

	// remote MD server. this can't fail. reconnection attempts
	// will be automatic.
	mdServer := NewMDServerRemote(config, mdserverAddr)
	return mdServer, nil
}

func makeKeyServer(config Config, serverRootDir *string, keyserverAddr string) (
	KeyServer, error) {
	if serverRootDir == nil {
		// local in-memory key server
		return NewKeyServerMemory(config)
	}

	if len(keyserverAddr) == 0 {
		// local persistent key server
		keyPath := filepath.Join(*serverRootDir, "kbfs_key")
		return NewKeyServerLocal(config, keyPath)
	}

	// currently the remote MD server also acts as the key server.
	keyServer := config.MDServer().(*MDServerRemote)
	return keyServer, nil
}

func makeBlockServer(config Config, serverRootDir *string, bserverAddr string) (
	BlockServer, error) {
	if len(bserverAddr) == 0 {
		if serverRootDir == nil {
			return NewBlockServerMemory(config)
		}

		blockPath := filepath.Join(*serverRootDir, "kbfs_block")
		return NewBlockServerLocal(config, blockPath)
	}

	fmt.Printf("Using remote bserver %s\n", bserverAddr)
	return NewBlockServerRemote(config, bserverAddr), nil
}

func makeKeybaseDaemon(config Config, serverRootDir *string, localUser libkb.NormalizedUsername, codec Codec, log logger.Logger) (KeybaseDaemon, error) {
	if localUser == "" {
		libkb.G.ConfigureSocketInfo()
		return NewKeybaseDaemonRPC(config, libkb.G, log), nil
	}

	users := []libkb.NormalizedUsername{"strib", "max", "chris", "fred"}
	userIndex := -1
	for i := range users {
		if localUser == users[i] {
			userIndex = i
			break
		}
	}
	if userIndex < 0 {
		return nil, fmt.Errorf("user %s not in list %v", localUser, users)
	}

	localUsers := MakeLocalUsers(users)

	// TODO: Auto-generate these, too?
	localUsers[0].Asserts = []string{"github:strib"}
	localUsers[1].Asserts = []string{"twitter:maxtaco"}
	localUsers[2].Asserts = []string{"twitter:malgorithms"}
	localUsers[3].Asserts = []string{"twitter:fakalin"}

	var localUID keybase1.UID
	if userIndex >= 0 {
		localUID = localUsers[userIndex].UID
	}

	if serverRootDir == nil {
		return NewKeybaseDaemonMemory(localUID, localUsers), nil
	}

	favPath := filepath.Join(*serverRootDir, "kbfs_favs")
	return NewKeybaseDaemonDisk(localUID, localUsers, favPath, codec)
}

// Init initializes a config and returns it. If localUser is
// non-empty, libkbfs does not communicate to any remote servers and
// instead uses fake implementations of various servers.
//
// If serverRootDir is nil, an in-memory server is used. If it is
// non-nil and points to the empty string, the current working
// directory is used. Otherwise, the pointed-to string is treated as a
// path.
//
// onInterruptFn is called whenever an interrupt signal is received
// (e.g., if the user hits Ctrl-C).
//
// Init should be called at the beginning of main. Shutdown (see
// below) should then be called at the end of main (usually via
// defer).
func Init(localUser libkb.NormalizedUsername, serverRootDir *string, cpuProfilePath,
	memProfilePath string, onInterruptFn func(), debug bool,
	bserverAddr, mdserverAddr string) (Config, error) {
	if cpuProfilePath != "" {
		// Let the GC/OS clean up the file handle.
		f, err := os.Create(cpuProfilePath)
		if err != nil {
			return nil, err
		}
		pprof.StartCPUProfile(f)
	}

	interruptChan := make(chan os.Signal, 1)
	signal.Notify(interruptChan, os.Interrupt)
	go func() {
		_ = <-interruptChan

		Shutdown(memProfilePath)

		if onInterruptFn != nil {
			onInterruptFn()
		}

		os.Exit(1)
	}()

	config := NewConfigLocal()

	// 64K blocks by default, block changes embedded max == 8K
	bsplitter, err := NewBlockSplitterSimple(64*1024, 8*1024,
		config.Codec())
	if err != nil {
		return nil, err
	}
	config.SetBlockSplitter(bsplitter)

	if registry := config.MetricsRegistry(); registry != nil {
		keyCache := config.KeyCache()
		keyCache = NewKeyCacheMeasured(keyCache, registry)
		config.SetKeyCache(keyCache)
	}

	// Set logging
	config.SetLoggerMaker(func(module string) logger.Logger {
		mname := "kbfs"
		if module != "" {
			mname += fmt.Sprintf("(%s)", module)
		}
		// Add log depth so that context-based messages get the right
		// file printed out.
		lg := logger.NewWithCallDepth(mname, 1)
		if debug {
			// Turn on debugging.  TODO: allow a proper log file and
			// style to be specified.
			lg.Configure("", true, "")
		}
		return lg
	})

	libkb.G.Init()
	libkb.G.ConfigureConfig()
	libkb.G.ConfigureLogging()
	libkb.G.ConfigureCaches()
	libkb.G.ConfigureMerkleClient()

	config.SetKeyManager(NewKeyManagerStandard(config))

	if libkb.G.Env.GetRunMode() == libkb.StagingRunMode &&
		strings.HasSuffix(bserverAddr, "dev.keybase.io:443") &&
		strings.HasSuffix(mdserverAddr, "dev.keybase.io:443") {
		config.SetRootCerts([]byte(DevRootCerts))
	} else if libkb.G.Env.GetRunMode() == libkb.ProductionRunMode &&
		strings.HasSuffix(bserverAddr, "kbfs.keybase.io:443") &&
		strings.HasSuffix(mdserverAddr, "kbfs.keybase.io:443") {
		config.SetRootCerts([]byte(ProductionRootCerts))
	}

	mdServer, err := makeMDServer(config, serverRootDir, mdserverAddr)
	if err != nil {
		return nil, fmt.Errorf("problem creating MD server: %v", err)
	}
	config.SetMDServer(mdServer)

	// note: the mdserver is the keyserver at the moment.
	keyServer, err := makeKeyServer(config, serverRootDir, mdserverAddr)
	if err != nil {
		return nil, fmt.Errorf("problem creating key server: %v", err)
	}

	if registry := config.MetricsRegistry(); registry != nil {
		keyServer = NewKeyServerMeasured(keyServer, registry)
	}

	config.SetKeyServer(keyServer)

	client.InitUI()
	if err := client.GlobUI.Configure(); err != nil {
		lg := logger.NewWithCallDepth("", 1)
		lg.Warning("problem configuring UI: %s", err)
		lg.Warning("ignoring for now...")
	}

	daemon, err := makeKeybaseDaemon(config, serverRootDir, localUser, config.Codec(), config.MakeLogger(""))
	if err != nil {
		return nil, fmt.Errorf("problem creating daemon: %s", err)
	}

	if registry := config.MetricsRegistry(); registry != nil {
		daemon = NewKeybaseDaemonMeasured(daemon, registry)
	}

	config.SetKeybaseDaemon(daemon)

	k := NewKBPKIClient(config)
	config.SetKBPKI(k)

	config.SetReporter(NewReporterKBPKI(config, 10, 1000))

	if localUser == "" {
		c := NewCryptoClient(config, libkb.G, config.MakeLogger(""))
		config.SetCrypto(c)
	} else {
		signingKey := MakeLocalUserSigningKeyOrBust(localUser)
		cryptPrivateKey := MakeLocalUserCryptPrivateKeyOrBust(localUser)
		config.SetCrypto(NewCryptoLocal(config, signingKey, cryptPrivateKey))
	}

	bserv, err := makeBlockServer(config, serverRootDir, bserverAddr)
	if err != nil {
		return nil, fmt.Errorf("cannot open block database: %v", err)
	}

	if registry := config.MetricsRegistry(); registry != nil {
		bserv = NewBlockServerMeasured(bserv, registry)
	}

	config.SetBlockServer(bserv)

	return config, nil
}

// Shutdown does any necessary shutdown tasks for libkbfs. Shutdown
// should be called at the end of main.
func Shutdown(memProfilePath string) error {
	pprof.StopCPUProfile()

	if memProfilePath != "" {
		// Let the GC/OS clean up the file handle.
		f, err := os.Create(memProfilePath)
		if err != nil {
			return err
		}

		pprof.WriteHeapProfile(f)
	}

	return nil
}
