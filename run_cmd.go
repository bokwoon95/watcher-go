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
	ExcludeRegexp *regexp.Regexp
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

func (cmd *RunCmd) Start() error {
	const debounceInterval = 500 * time.Millisecond
	if !atomic.CompareAndSwapInt32(&cmd.started, 0, 1) {
		return fmt.Errorf("already started")
	}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	cmd.programPath = filepath.Join(os.TempDir(), "main"+time.Now().Format("20060102150405"))
	if cmd.Output != "" {
		cmd.programPath = cmd.Output
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(cmd.programPath, ".exe") {
		cmd.programPath += ".exe"
	}
	watched := make(map[string]struct{})
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	addDirsRecursively(watcher, watched, ".")
	buildArgs := make([]string, 0, len(cmd.BuildFlags)+4)
	buildArgs = append(buildArgs, "build", "-o", cmd.programPath)
	buildArgs = append(buildArgs, cmd.BuildFlags...)
	buildArgs = append(buildArgs, cmd.Package)
	var program *exec.Cmd
	var buildCmd *exec.Cmd
	var event fsnotify.Event
	var ok, rebuild bool
	// Build + Run Loop.
	for {
		// Stop program and any child processes.
		if program != nil {
			stop(program)
			_ = os.Remove(cmd.programPath)
		}
		// Set program to nil; it will be non-nil once the buildCmd succeeds.
		program = nil
		// Build program.
		buildCmd = exec.Command("go", buildArgs...)
		buildCmd.Stdout = cmd.Stdout
		buildCmd.Stderr = cmd.Stderr
		err = buildCmd.Run()
		if err == nil {
			// Run program.
			program = exec.Command(cmd.programPath, cmd.Args...)
			program.Stdout = cmd.Stdout
			program.Stderr = cmd.Stderr
			go program.Run()
		}
		// Run  Loop.
		rebuild = false
		for rebuild == false {
			select {
			case err = <-watcher.Errors:
				fmt.Fprintln(cmd.Stderr, err)
			case event, ok = <-watcher.Events:
				if !ok {
					return nil // cmd.Stop() was called which called watcher.Close().
				}
				if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) && !event.Has(fsnotify.Remove) {
					continue
				}
				if !isDir(event.Name) {
					if isValid(cmd.IncludeRegexp, cmd.ExcludeRegexp, event.Name) {
						fmt.Println(event)
						rebuild = true
						break
					}
					continue
				}
				if event.Has(fsnotify.Create) {
					addDirsRecursively(watcher, watched, event.Name)
					continue
				}
				if event.Has(fsnotify.Remove) {
					removeDirsRecursively(watcher, watched, event.Name)
					continue
				}
			}
		}
	}
}

func (cmd *RunCmd) Stop() {
	if cmd.watcher != nil {
		_ = cmd.watcher.Close()
	}
	_ = os.Remove(cmd.programPath)
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
