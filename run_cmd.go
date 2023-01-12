package wgo

import (
	"bytes"
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
	"unicode/utf8"

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
	Package                string
	Env                    []string  // TODO: write a test to see if env variables passed to wgo are propagated to the program. https://github.com/cosmtrek/air/issues/58 full_bin SUCKS! using the live env vars is better! make sure it works. https://github.com/cosmtrek/air/pull/166 Is it possible to set env vars in one line on windows and pass them in to wgo? https://github.com/cosmtrek/air/issues/338 https://superuser.com/q/1405650 Maybe don't bother with the one line requirement, that is needed for air because it runs a single line command from its config file whereas wgo is able to access the shell in its entirety. If people want to source a dev.env file before running wgo they can (but make sure that works) https://github.com/cosmtrek/air/issues/77
	Dir                    string    // TODO: reintroduce Dir string so that people can cd to a different directory, watch different files outside the project root and have peace of mind that they are able to run the binary in a completely different directory. https://github.com/cosmtrek/air/issues/85
	Stdin                  io.Reader // TODO: need to test if it is possible to type rs<Enter> on macOS and if so implement nodemon's custom restart command. https://github.com/cosmtrek/air/issues/351
	Stdout                 io.Writer
	Stderr                 io.Writer
	BuildFlags             []string
	Output                 string           // TODO: when I include the -o flag it hangs. Why?
	Args                   []string         // TODO: oh hell no, go run uses -- as the args separator. Need to rearchitect the application already.
	DirRegexps             []*regexp.Regexp // TODO: Exclude and Include don't seem to work. Figure out how to get multiple specific directories to work. Figure out how to only watch files in the root folder nonrecursively. There may come a time where *.{go,html,tmpl,tpl} is no longer a sane default and users are often asking for css,js, or even more madness. Or even a general purpose task runner. But stay strong, because wgo run never seeks to achieve what go run couldn't already (it simply adds a file watcher to go run). (Are regexes rich enough to support omitting *_test.go? https://github.com/cosmtrek/air/issues/127)
	FileRegexps            []*regexp.Regexp
	FilepathRegexps        []*regexp.Regexp // TODO: normalize all filepath separators to forward slash.
	ExcludeDirRegexps      []*regexp.Regexp
	ExcludeFileRegexps     []*regexp.Regexp
	ExcludeFilepathRegexps []*regexp.Regexp
	watcher                *fsnotify.Watcher
	started                int32
	programPath            string
}

func RunCommand(args ...string) (*RunCmd, error) {
	cmd := RunCmd{
		BuildFlags: make([]string, 0, len(buildFlags)*2),
	}
	var dirs, files, filepaths, xdirs, xfiles, xfilepaths []string
	flagset := flag.NewFlagSet("", flag.ContinueOnError)
	flagset.StringVar(&cmd.Output, "o", "", "")
	flagset.Func("dir", "", func(value string) error {
		dirs = append(dirs, value)
		return nil
	})
	flagset.Func("files", "", func(value string) error {
		files = append(files, value)
		return nil
	})
	flagset.Func("filepaths", "", func(value string) error {
		filepaths = append(filepaths, value)
		return nil
	})
	flagset.Func("xdirs", "", func(value string) error {
		xdirs = append(xdirs, value)
		return nil
	})
	flagset.Func("xfiles", "", func(value string) error {
		xfiles = append(xfiles, value)
		return nil
	})
	flagset.Func("xfilepaths", "", func(value string) error {
		xfilepaths = append(xfilepaths, value)
		return nil
	})
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
	cmd.DirRegexps, err = compileRegexps(dirs)
	if err != nil {
		return nil, err
	}
	cmd.FileRegexps, err = compileRegexps(files)
	if err != nil {
		return nil, err
	}
	cmd.FilepathRegexps, err = compileRegexps(filepaths)
	if err != nil {
		return nil, err
	}
	cmd.ExcludeDirRegexps, err = compileRegexps(xdirs)
	if err != nil {
		return nil, err
	}
	cmd.ExcludeFileRegexps, err = compileRegexps(xfiles)
	if err != nil {
		return nil, err
	}
	cmd.ExcludeFilepathRegexps, err = compileRegexps(xfilepaths)
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

func (cmd *RunCmd) Start() {
	if !atomic.CompareAndSwapInt32(&cmd.started, 0, 1) {
		// Start() should only run once, subsequent calls to Start() are
		// ignored.
		return
	}
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
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
	addDirsRecursively(watcher, watched, cmd.DirRegexps, cmd.ExcludeDirRegexps, ".")
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
	// TODO: after the debouncing, maybe we need an intermediary channel that
	// does non-blocking sends in order to purposely drop events while the
	// build command is running so that we don't receive a glut of backed-up
	// events once the build command is over. But need to investigate if needed
	// first, because if everything seems to work without that intermediary
	// channel I'd rather not add it.
	timer := time.NewTimer(0)
	// Drain the initial timer event so that it doesn't count to the first
	// iteration of the for-select loop.
	<-timer.C
	var program *exec.Cmd

	// Clean + Build + Run cycle.
	for {
		// Clean up the program (if exists) and any child processes.
		if program != nil && program.Process != nil {
			cleanup(program)
		}
		// Set the program to nil; it will be non-nil if buildCmd succeeds
		// without error.
		// program = nil // if we're checking program.Process we don't need to set program to nil anymore to indicate the command has stopped.
		// Build the program (piping its stdout and stderr to cmd.Stdout and
		// cmd.Stderr).
		buildCmd := exec.Command("go", buildArgs...)
		buildCmd.Env = cmd.Env
		buildCmd.Stdout = cmd.Stdout
		buildCmd.Stderr = cmd.Stderr
		err = buildCmd.Run()
		// if errors.Is(err, context.Cancelled) || errors.Is(err, context.DeadlineExceeded) { return }
		if err == nil {
			// Run the program in the background (piping its stdout and stderr to
			// cmd.Stdout and cmd.Stderr).
			program = exec.Command(cmd.programPath, cmd.Args...)
			program.Env = cmd.Env
			program.Stdin = cmd.Stdin
			program.Stdout = cmd.Stdout
			program.Stderr = cmd.Stderr
			setpgid(program)
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
					// TODO: cleanup(program) if program is running, otherwise
					// we are forcibly ending main() and killing the goroutine
					// (potentially not letting the server do its cleanup).
					// TODO: instead of returning here, craft our own <-done:
					// channel to make it really clear how this loop is
					// exiting. Then we don't have to check for the zero value
					// from the events channel or whatever.
					return // cmd.Stop() was called which called watcher.Close().
				}
				// We're only interested in Create | Write | Remove events,
				// ignore everything else.
				if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) && !event.Has(fsnotify.Remove) {
					continue
				}
				if !isDir(event.Name) {
					if isValid(cmd.FileRegexps, cmd.FilepathRegexps, cmd.ExcludeFileRegexps, cmd.ExcludeFilepathRegexps, event.Name) {
						timer.Reset(500 * time.Millisecond) // Start the timer.
					}
					continue
				}
				// If a directory was created, recursively add every directory
				// inside it to the watcher.
				if event.Has(fsnotify.Create) {
					addDirsRecursively(watcher, watched, cmd.DirRegexps, cmd.ExcludeDirRegexps, event.Name)
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
	if atomic.LoadInt32(&cmd.started) == 0 {
		// If Start() hasn't been called, do nothing.
		return
	}
	if cmd.watcher != nil {
		_ = cmd.watcher.Close()
	}
	if cmd.programPath != "" {
		_ = os.Remove(cmd.programPath)
	}
}

func (cmd *RunCmd) Run() (exitCode int) {
	return 0
}

func compileRegexps(patterns []string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	regexps := make([]*regexp.Regexp, len(patterns))
	buf := &bytes.Buffer{}
	var err error
	for i, pattern := range patterns {
		if strings.Count(pattern, ".") == 0 {
			regexps[i], err = regexp.Compile(pattern)
			if err != nil {
				return regexps, err
			}
			continue
		}
		if buf.Cap() < len(pattern) {
			buf.Grow(len(pattern))
		}
		buf.Reset()
		for j := 0; j < len(pattern); {
			prev, _ := utf8.DecodeLastRune(buf.Bytes())
			curr, width := utf8.DecodeRuneInString(pattern[j:])
			next, _ := utf8.DecodeRuneInString(pattern[j+width:])
			j += width
			if prev != '\\' && curr == '.' && (('a' <= next && next <= 'z') || ('A' <= next && next <= 'Z')) {
				buf.WriteString("\\.")
			} else {
				buf.WriteRune(curr)
			}
		}
		regexps[i], err = regexp.Compile(buf.String())
		if err != nil {
			return regexps, err
		}
	}
	return regexps, nil
}

func isDir(path string) bool {
	fileinfo, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fileinfo.IsDir()
}

func isValid(fileRegexps, filepathRegexps, excludeFileRegexps, excludeFilepathRegexps []*regexp.Regexp, path string) bool {
	basename := filepath.Base(path)
	normalizedPath := filepath.ToSlash(path)
	for _, r := range excludeFileRegexps {
		if r.MatchString(basename) {
			return false
		}
	}
	for _, r := range excludeFilepathRegexps {
		if r.MatchString(normalizedPath) {
			return false
		}
	}
	for _, r := range fileRegexps {
		if r.MatchString(basename) {
			return true
		}
	}
	for _, r := range filepathRegexps {
		if r.MatchString(normalizedPath) {
			return true
		}
	}
	if strings.HasSuffix(path, ".go") {
		return true
	}
	return false
}

// TODO: check if newly added directories are watched (as well as their subdirectories).
func addDirsRecursively(watcher *fsnotify.Watcher, watched map[string]struct{}, dirRegexps, excludeDirRegexps []*regexp.Regexp, dir string) {
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
		basename := filepath.Base(path)
		if basename == ".git" || basename == ".hg" || basename == ".idea" || basename == ".vscode" || basename == ".settings" {
			return filepath.SkipDir
		}
		normalizedPath := filepath.ToSlash(path)
		for _, r := range excludeDirRegexps {
			if r.MatchString(normalizedPath) {
				return filepath.SkipDir
			}
		}
		if len(dirRegexps) > 0 {
			matched := false
			for _, r := range dirRegexps {
				if r.MatchString(normalizedPath) {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}
		err = watcher.Add(path)
		if err == nil {
			watched[path] = struct{}{}
		}
		return nil
	})
}

// TODO: check if newly removed directories are removed (as well as their subdirectories).
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
