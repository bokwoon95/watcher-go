package wgo

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Copied from `go help build`.
var buildFlags = [...]string{
	"a", "n", "p", "race", "msan", "asan", "v", "work", "x", "asmflags",
	"buildmode", "buildvcs", "compiler", "gccgoflags", "gcflags",
	"installsuffix", "ldflags", "linkshared", "mod", "modcacherw", "modfile",
	"overlay", "pkgdir", "tags", "trimpath", "toolexec",
}

type RunCmd struct {
	// (Required)
	Package       string
	Stdout        io.Writer
	Stderr        io.Writer
	BuildFlags    []string
	Output        string // TODO: when I include the -o flag it hangs. Why?
	Args          []string
	ExcludeRegexp *regexp.Regexp // TODO: Exclude and Include don't seem to work.
	IncludeRegexp *regexp.Regexp
	watcher       *fsnotify.Watcher
	started       int32
	programPath   string
}

func RunCommand(args ...string) (*RunCmd, error) {
	cmd := RunCmd{
		BuildFlags: make([]string, 0, len(buildFlags)*2),
	}
	var include, exclude string
	flagset := flag.NewFlagSet("", flag.ContinueOnError)
	flagset.StringVar(&cmd.Output, "o", "", "The -o flag in go build.")
	flagset.StringVar(&exclude, "exclude", "", "Regexp that matches excluded files.")
	flagset.StringVar(&include, "include", "", "Regexp that matches included files.")
	for i := range buildFlags {
		name := buildFlags[i]
		flagset.Func(name, "The -"+name+" flag in go build.", func(value string) error {
			switch name {
			case "a", "n", "race", "msan", "asan", "v", "work", "x",
				"buildvcs", "linkshared", "modcacherw", "trimpath": // bool flags
				if value == "" {
					value = "true"
				}
			}
			cmd.BuildFlags = append(cmd.BuildFlags, "-"+name, value)
			return nil
		})
	}
	flagset.Usage = func() {
		fmt.Fprint(flagset.Output(), `Build and run the package, rebuilding and rerunning whenever *.{go,html,tmpl,tpl} files change.
Usage:
  wgo run [BUILD_FLAGS...] <package_or_file> [ARGS...]
  wgo run main.go
  wgo run .
  wgo run -tags=fts5 ./cmd/main
  wgo run -tags=fts5 ./cmd/main arg1 arg2 arg3
Flags:
  Any flag that works with 'go build' works here.
  -exclude
        A regexp that matches excluded files. This works in conjuction with the
        *.{go,html,tmpl,tpl} pattern.
  -include
        A regexp that matches included files. If provided, this overrides the
        *.{go,html,tmpl,tpl} pattern. You will have to include go files
        yourself using the regex.
`)
	}
	err := flagset.Parse(args)
	if err != nil {
		return nil, err
	}
	flagArgs := flagset.Args()
	if len(flagArgs) == 0 {
		return nil, fmt.Errorf("package or file not provided")
	}
	if exclude != "" {
		cmd.ExcludeRegexp, err = regexp.Compile(exclude)
		if err != nil {
			return nil, fmt.Errorf("-exclude %q: %s", exclude, err)
		}
	}
	if include != "" {
		cmd.IncludeRegexp, err = regexp.Compile(include)
		if err != nil {
			return nil, fmt.Errorf("-include %q: %s", include, err)
		}
	}
	cmd.Package, cmd.Args = flagArgs[0], flagArgs[1:]
	return &cmd, nil
}

func (cmd *RunCmd) Start() {
	// Prevent Start() from running more than once by ignoring subsequent calls
	// to Start().
	if !atomic.CompareAndSwapInt32(&cmd.started, 0, 1) {
		return
	}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	// Create a temp path for the program by default, unless the user specified
	// a custom output.
	cmd.programPath = filepath.Join(os.TempDir(), "main"+time.Now().Format("20060102150405"))
	if cmd.Output != "" {
		cmd.programPath = cmd.Output
	}
	// Windows refuses to run programs without an .exe extension, add it for
	// the user if they didn't include it.
	if runtime.GOOS == "windows" && !strings.HasSuffix(cmd.programPath, ".exe") {
		cmd.programPath += ".exe"
	}
	// 'watched' tracks which dirs are currently present in the watcher.
	watched := make(map[string]struct{})
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintln(cmd.Stderr, err)
		return
	}
	addDirsRecursively(watcher, watched, ".")
	// go build -o <programPath> [BUILD_FLAGS...] <package>
	buildArgs := make([]string, 0, len(cmd.BuildFlags)+4)
	buildArgs = append(buildArgs, "build", "-o", cmd.programPath)
	buildArgs = append(buildArgs, cmd.BuildFlags...)
	buildArgs = append(buildArgs, cmd.Package)
	// The timer is used to debounce events. When a valid event arrives, it
	// starts the timer. Only when the timer expires does it actually kick off
	// a clean + build + run cycle. This means events that come in too quickly
	// will keep resetting the timer over and over without actually triggering
	// a rerun (the timer must be allowed to fully expire first).
	timer := time.NewTimer(0)
	// Drain the initial timer event so that it doesn't count to the first
	// iteration of the for-select loop.
	<-timer.C
	var program *exec.Cmd

	// Clean + Build + Run cycle.
	for {
		// Clean up the program (if exists) and any child processes.
		if program != nil {
			cleanup(program)
		}
		// Set the program to nil; it will be non-nil if buildCmd succeeds
		// without error.
		program = nil
		// Build the program (piping its stdout and stderr to cmd.Stdout and
		// cmd.Stderr).
		buildCmd := exec.Command("go", buildArgs...)
		buildCmd.Stdout = cmd.Stdout
		buildCmd.Stderr = cmd.Stderr
		err = buildCmd.Run()
		if err == nil {
			// Run the program in the background (piping its stdout and stderr to
			// cmd.Stdout and cmd.Stderr).
			program = exec.Command(cmd.programPath, cmd.Args...)
			setpgid(program)
			program.Stdout = cmd.Stdout
			program.Stderr = cmd.Stderr
			go program.Run()
		}
		// Wait for file events. When a valid event comes in 'rebuild' will be
		// set to true, breaking the wait loop and initiating another clean +
		// build + run cycle.
		rebuild := false
		for rebuild == false {
			select {
			case err = <-watcher.Errors:
				fmt.Fprintln(cmd.Stderr, err)
			case event, ok := <-watcher.Events:
				if !ok {
					return // cmd.Stop() was called which called watcher.Close().
				}
				// We're only interested in Create | Write | Remove events,
				// ignore everything else.
				if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) && !event.Has(fsnotify.Remove) {
					continue
				}
				if !isDir(event.Name) {
					if isValid(cmd.IncludeRegexp, cmd.ExcludeRegexp, event.Name) {
						timer.Reset(500 * time.Millisecond) // Start the timer.
					}
					continue
				}
				// If a directory was created, recursively add every directory
				// inside it to the watcher.
				if event.Has(fsnotify.Create) {
					addDirsRecursively(watcher, watched, event.Name)
					continue
				}
				// If a directory was removed, recursively remove every
				// directory inside it from the watcher.
				if event.Has(fsnotify.Remove) {
					removeDirsRecursively(watcher, watched, event.Name)
					continue
				}
			case <-timer.C: // Timer expired, start the rebuild.
				rebuild = true
				break
			}
		}
	}
}

func (cmd *RunCmd) Stop() {
	// Do nothing if Start() hasn't yet been called.
	if atomic.LoadInt32(&cmd.started) == 0 {
		return
	}
	if cmd.watcher != nil {
		_ = cmd.watcher.Close()
	}
	if cmd.programPath != "" {
		_ = os.Remove(cmd.programPath)
	}
}

func isDir(path string) bool {
	fileinfo, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fileinfo.IsDir()
}

func isValid(exclude, include *regexp.Regexp, path string) bool {
	if exclude != nil && exclude.MatchString(path) {
		return false
	}
	if include != nil {
		if include.MatchString(path) {
			return true
		}
		return false
	}
	if strings.HasPrefix(path, ".") {
		return false
	}
	ext := filepath.Ext(path)
	if ext == ".go" || ext == ".html" || ext == ".tmpl" || ext == ".tpl" {
		return true
	}
	return false
}

func addDirsRecursively(watcher *fsnotify.Watcher, watched map[string]struct{}, dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if _, ok := watched[path]; ok {
			return nil
		}
		err = watcher.Add(path)
		if err == nil {
			watched[path] = struct{}{}
		}
		return nil
	})
}

func removeDirsRecursively(watcher *fsnotify.Watcher, watched map[string]struct{}, dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if _, ok := watched[path]; !ok {
			return nil
		}
		err = watcher.Remove(path)
		if err == nil {
			delete(watched, path)
		}
		return nil
	})
}
