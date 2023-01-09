package wgo

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Copied from `go help build`.
var buildFlags = [...]string{
	"a", // bool
	"n", // bool
	"p",
	"race", // bool
	"msan", // bool
	"asan", // bool
	"v",    // bool
	"work", // bool
	"x",    // bool
	"asmflags",
	"buildmode",
	"buildvcs", // bool
	"compiler",
	"gccgoflags",
	"gcflags",
	"installsuffix",
	"ldflags",
	"linkshared", // bool
	"mod",
	"modcacherw", // bool
	"modfile",
	"overlay",
	"pkgdir",
	"tags",
	"trimpath", // bool
	"toolexec",
}

type RunCmd struct {
	Stdout     io.Writer
	Stderr     io.Writer
	BuildFlags []string
	Output     string
	Package    string
	Args       []string
}

func RunCommand(args ...string) (*RunCmd, error) {
	cmd := RunCmd{
		BuildFlags: make([]string, 0, len(buildFlags)*2),
	}
	flagset := flag.NewFlagSet("", flag.ContinueOnError)
	flagset.StringVar(&cmd.Output, "o", "", "The -o flag in go build.")
	for i := range buildFlags {
		name := buildFlags[i]
		flagset.Func(name, "The -"+name+" flag in go build.", func(value string) error {
			switch name {
			case "a", "n", "race", "msan", "asan", "v", "work", "x", "buildvcs", "linkshared", "modcacherw", "trimpath":
				if value == "" {
					value = "true"
				}
			}
			cmd.BuildFlags = append(cmd.BuildFlags, "-"+name, value)
			return nil
		})
	}
	err := flagset.Parse(args)
	if err != nil {
		return nil, err
	}
	flagArgs := flagset.Args()
	if len(flagArgs) == 0 {
		return nil, fmt.Errorf("package or file not provided")
	}
	cmd.Package, cmd.Args = flagArgs[0], flagArgs[1:]
	return &cmd, nil
}

func (cmd *RunCmd) Run() error {
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	output := filepath.Join(os.TempDir(), "main"+time.Now().Format("20060102150405"))
	if cmd.Output != "" {
		output = cmd.Output
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(output, ".exe") {
		output += ".exe"
	}
	defer func() {
		if output != "" && cmd.Output == "" {
			_ = os.Remove(output)
		}
	}()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	// go build [BUILD_FLAGS...] <package>
	buildArgs := make([]string, 0, len(cmd.BuildFlags)+4)
	buildArgs = append(buildArgs, "build", "-o", output)
	buildArgs = append(buildArgs, cmd.BuildFlags...)
	buildArgs = append(buildArgs, cmd.Package)
	var runCmd *exec.Cmd
	for {
		// kill cmd
		if runCmd != nil {
			pid := runCmd.Process.Pid
			switch runtime.GOOS {
			case "windows":
				killCmd := exec.Command("TASKKILL", "/T", "/F", "/PID", strconv.Itoa(pid))
				err = killCmd.Run()
				if err != nil {
					return err
				}
			case "darwin":
				// https://stackoverflow.com/questions/22470193/why-wont-go-kill-a-child-process-correctly
				err = syscall.Kill(-pid, syscall.SIGKILL)
				// Wait releases any resources associated with the Process.
				_, _ = runCmd.Process.Wait()
			default:
			}
		}
		// build bin
		// run bin
	}
}
