package main

import (
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/xtls/xray-core/common/cmdarg"
	"github.com/xtls/xray-core/common/errors"
	clog "github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/main/commands/base"
)

var cmdRun = &base.Command{
	UsageLine: "{{.Exec}} run [-c config.json] [-confdir dir]",
	Short:     "Run Xray with config, the default command",
	Long: `
Run Xray with config, the default command.

The -config=file, -c=file flags set the config files for 
Xray. Multiple assign is accepted.

The -confdir=dir flag sets a dir with multiple json config

The -format=json flag sets the format of config files. 
Default "auto".

The -test flag tells Xray to test config files only, 
without launching the server.

The -dump flag tells Xray to print the merged config.
	`,
}

func init() {
	cmdRun.Run = executeRun // break init loop
	log.SetFlags(log.Ldate | log.Ltime)
}

var (
	configFiles cmdarg.Arg // "Config file for Xray.", the option is customed type, parse in main
	configDir   string
	dump        = cmdRun.Flag.Bool("dump", false, "Dump merged config only, without launching Xray server.")
	test        = cmdRun.Flag.Bool("test", false, "Test config file only, without launching Xray server.")
	format      = cmdRun.Flag.String("format", "auto", "Format of input file.")

	/* We have to do this here because Golang's Test will also need to parse flag, before
	 * main func in this file is run.
	 */
	_ = func() bool {
		cmdRun.Flag.Var(&configFiles, "config", "Config path for Xray.")
		cmdRun.Flag.Var(&configFiles, "c", "Short alias of -config")
		cmdRun.Flag.StringVar(&configDir, "confdir", "", "A dir with multiple json config")

		return true
	}()
)

func executeRun(cmd *base.Command, args []string) {
	if *dump { // dump 解析用的
		clog.ReplaceWithSeverityLogger(clog.Severity_Warning)
		errCode := dumpConfig()
		os.Exit(errCode)
	}

	printVersion() // 打印版本号core.go 里面
	server, err := startXray()
	if err != nil {
		log.Println("Failed to start:", err)
		// Configuration error. Exit with a special value to prevent systemd from restarting.
		os.Exit(23)
	}

	if *test {
		log.Println("Configuration OK.")
		os.Exit(0)
	}

	if err := server.Start(); err != nil {
		log.Println("Failed to start:", err)
		os.Exit(-1)
	}

	// Explicitly triggering GC to remove garbage from config loading.
	runtime.GC()
	debug.FreeOSMemory()

	// Channel for server restart
	restartChan := make(chan struct{})

	// Start file watcher for config files
	configFiles := getConfigFilePath(true)
	watcher, err := startFileWatcher(configFiles, restartChan)
	if err != nil {
		log.Println("Warning: Failed to start file watcher:", err)
		log.Println("Config file changes will not trigger automatic restart")
	}

	// Goroutine for server restart
	go func() {
		for {
			<-restartChan
			log.Println("[Auto Restart] Restarting Xray due to config change...")

			// Close current server
			if err := server.Close(); err != nil {
				log.Println("[Auto Restart] Error closing server:", err)
			}

			// Start new server
			newServer, err := startXray()
			if err != nil {
				log.Println("[Auto Restart] Failed to load new config:", err)
				log.Println("[Auto Restart] Keeping current server running")
				continue
			}

			if err := newServer.Start(); err != nil {
				log.Println("[Auto Restart] Failed to start new server:", err)
				log.Println("[Auto Restart] Keeping current server running")
				continue
			}

			// Replace server instance
			server = newServer
			log.Println("[Auto Restart] Xray restarted successfully with new config")
		}
	}()

	// Main signal handling loop
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM)

	log.Println("Xray started successfully. Press Ctrl+C to stop.")

	for {
		select {
		case <-osSignals:
			log.Println("Shutting down...")
			if watcher != nil {
				watcher.Close()
			}
			if err := server.Close(); err != nil {
				log.Println("Error closing server:", err)
			}
			os.Exit(0)
		}
	}
}

func startFileWatcher(configFiles cmdarg.Arg, restartChan chan struct{}) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	// Watch each config file
	for _, file := range configFiles {
		if file != "stdin:" {
			// Get absolute path
			absPath, err := filepath.Abs(file)
			if err != nil {
				log.Println("Error getting absolute path for", file, ":", err)
				continue
			}

			// Watch the file
			if err := watcher.Add(absPath); err != nil {
				log.Println("Error watching", absPath, ":", err)
				continue
			}
			log.Println("Watching config file:", absPath)
		}
	}

	// Goroutine to handle file changes
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				// Only react to write or create events
				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					log.Println("Config file changed:", event.Name)
					// Trigger restart
					restartChan <- struct{}{}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("File watcher error:", err)
			}
		}
	}()

	return watcher, nil
}

func dumpConfig() int {
	files := getConfigFilePath(false)
	if config, err := core.GetMergedConfig(files); err != nil {
		log.Println(err)
		time.Sleep(1 * time.Second)
		return 23
	} else {
		log.Print(config)
	}
	return 0
}

func fileExists(file string) bool {
	info, err := os.Stat(file)
	return err == nil && !info.IsDir()
}

func dirExists(file string) bool {
	if file == "" {
		return false
	}
	info, err := os.Stat(file)
	return err == nil && info.IsDir()
}

func getRegepxByFormat() string {
	return `^.+\.(json|jsonc)$`
}

func readConfDir(dirPath string) {
	confs, err := os.ReadDir(dirPath)
	if err != nil {
		log.Fatalln(err)
	}
	for _, f := range confs {
		matched, err := regexp.MatchString(getRegepxByFormat(), f.Name())
		if err != nil {
			log.Fatalln(err)
		}
		if matched {
			configFiles.Set(path.Join(dirPath, f.Name()))
		}
	}
}

func getConfigFilePath(verbose bool) cmdarg.Arg {
	if dirExists(configDir) {
		if verbose {
			log.Println("Using confdir from arg:", configDir)
		}
		readConfDir(configDir)
	} else if envConfDir := platform.GetConfDirPath(); dirExists(envConfDir) {
		if verbose {
			log.Println("Using confdir from env:", envConfDir)
		}
		readConfDir(envConfDir)
	}

	if len(configFiles) > 0 {
		return configFiles
	}

	if workingDir, err := os.Getwd(); err == nil {
		log.Println("workingDir: ", workingDir)
		configFile := filepath.Join(workingDir, "config.json")
		if fileExists(configFile) {
			if verbose {
				log.Println("Using default config: ", configFile)
			}
			return cmdarg.Arg{configFile}
		}
	}

	if configFile := platform.GetConfigurationPath(); fileExists(configFile) {
		if verbose {
			log.Println("Using config from env: ", configFile)
		}
		return cmdarg.Arg{configFile}
	}

	if verbose {
		log.Println("Using config from STDIN")
	}
	return cmdarg.Arg{"stdin:"}
}

func getConfigFormat() string {
	f := core.GetFormatByExtension(*format)
	if f == "" {
		f = "auto"
	}
	return f
}

func startXray() (core.Server, error) {
	configFiles := getConfigFilePath(true)

	// config, err := core.LoadConfig(getConfigFormat(), configFiles[0], configFiles)

	c, err := core.LoadConfig(getConfigFormat(), configFiles)
	if err != nil {
		return nil, errors.New("failed to load config files: [", configFiles.String(), "]").Base(err)
	}

	server, err := core.New(c)
	if err != nil {
		return nil, errors.New("failed to create server").Base(err)
	}

	return server, nil
}
