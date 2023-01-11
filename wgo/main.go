package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	wgo "github.com/bokwoon95/watcher-go"
)

const helptext = `Usage:
  wgo run [BUILD_FLAGS...] <package_or_file> [ARGS...] # Build and run the package, rebuilding and rerunning whenever *.{go,html,tmpl,tpl} files change.
Example:
  wgo run main.go
  wgo run .
  wgo run -tags=fts5 ./cmd/main
  wgo run -tags=fts5 ./cmd/main arg1 arg2 arg3

Run wgo run -h for more details about specific flags.
`

func main() {
	if len(os.Args) == 1 {
		fmt.Print(helptext)
		os.Exit(0)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "run":
		runCmd, err := wgo.RunCommand(args...)
		if err != nil {
			exit(cmd, err)
		}
		go runCmd.Start()
		<-sigs
		runCmd.Stop()
	default:
		fmt.Println("wgo " + cmd + ": unknown command")
		fmt.Println("Run 'wgo' for usage.")
		os.Exit(1)
	}
}

func exit(cmd string, err error) {
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, cmd+": "+err.Error())
	os.Exit(1)
}
