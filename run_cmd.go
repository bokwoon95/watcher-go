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

// TODO: StartService bool, which checks if the output binary already exists and runs that first (and then waiting on the file even loop). https://github.com/cosmtrek/air/issues/126 Idea is to retain the oldProgramPath, build a new programPath and if it succeeds then run the new programPath and cleanup the oldProgramPath otherwise leave the oldProgramPath running.
// TODO: PropagateSigint bool, which propagates SIGINT to the underlying binary in a exponential backoff retry loop until it's done or until 10 tries have been up at which point the service is forcibly killed. https://github.com/cosmtrek/air/pull/49 But sometimes people may want a hard cap on the time waited, so provide an option for that as well. With this option it will be possible to use wgo to run servers in production, together with the feature of falling back on the old binary in case the new build fails. https://github.com/cosmtrek/air/issues/129 Do NOT do any SMTP mail handling because build failure notifications is a job for a proper CI/CD system. (Wow people may actually like exponential backoff retry huh if their app takes fucking ages to close down after sigkill. what's the difference between sigkill and sigint and sigterm and sighup? https://github.com/cosmtrek/air/issues/29)
// TODO: experiment if bash backgroundjobs & can multiplex multiple wgo calls in one stdout. https://github.com/cosmtrek/air/issues/160
// TODO: add disclaimer saying I don't work with Docker and I dont know shit about virtual filesystems or how fsnotify works so if you file a report I likely can't help you until someone kind enough does.
// TODO: test if wgo handles port :8080 just fine (and maybe try it on a remote box and see if it's reachable from outside). Since apparently that's what people do on live, they use air to live-compile their codebase on the server so that they can just push and see changes immediately. Means need to try git pulling and seeing if wgo automatically picks it up and recompiles the app. That also means need an option to fall back on the old binary in case the new build fails (can't just os.Remove before building, need a different flow). https://github.com/cosmtrek/air/issues/311
// TODO: does calling Fatal in an application not free up a port while wgo is running? Should I kill wgo if no build program is running? Liveliness checks? Too much for wgo? https://github.com/cosmtrek/air/issues/116 (I'm leaning towards killing wgo if the underlying program dies on its own, so that orchestrators can detect that wgo is dead and spin up another instance https://github.com/cosmtrek/air/issues/353)
// TODO: wtf how is it possible to implement live reload with delve? https://github.com/cosmtrek/air/issues/76
// TODO: ok fine I'll investigate running in docker https://dev.to/andreidascalu/setup-go-with-vscode-in-docker-for-debugging-24ch
// TODO: there may come a day where people require reading files from outside the root directory for whatever reason https://github.com/cosmtrek/air/issues/41 (UGH fine, you can have your arbitrary file watching and rebuild server https://github.com/cosmtrek/air/issues/40 (Would it be possible to add include_dir which would be used instead of exclude_dir. We have a monolithic repository and it would be easiest to have it watch the root and only include directories the project needs. Thanks!))
// TODO: okay, okay. generic project runner? if a file in /assets changes, rebuild using a custom command? https://github.com/cosmtrek/air/issues/14 (Note, a generic task runner means you would have to look into getting wgo into systems via package managers on windows, linux and macos... GoReleaser?)
// NOTE: NO multiple commands and splicing with colorful sidebars like nodemon! The structure of a wgo run must be simple. Watch files, run server. Or watch files, build program. Or watch files, run arbitrary command. If you need to splice, compose it together using bash's support for background jobs and run multiple wgos in the same session.
// TODO: figure out how to use wgo in Dockerfile + Docker Compose https://github.com/cosmtrek/air/issues/54
// TODO: use CommandContext and runCmd.Stop() should propogate cancellation to underlying build and run commands https://github.com/cosmtrek/air/issues/127
// TODO: probably add a wgo dlv to rerun dlv on file change. need to figure out how dlv runs headlessly and how clients connect to it (which clients? goland?) https://github.com/cosmtrek/air/issues/216#issuecomment-982348931 (https://github.com/cosmtrek/air/issues/241)
// NOTE: There's a fine line between what wgo does and what nodemon does. wgo is as convenient as it is because it knows/can control the output programPath to run with for the Go language. But other task runners usually require running a custom command. Maybe wgo watch [FLAGS...] <path> -- <command> [ARGS...]? What about using stdin to pipe in a newline separated list of files? Don't ask users to fuck around with esoteric find flags?
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
	// Prevent Start() from running more than once by ignoring subsequent calls
	// to Start().
	if !atomic.CompareAndSwapInt32(&cmd.started, 0, 1) {
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
		if program != nil {
			cleanup(program)
		}
		// Set the program to nil; it will be non-nil if buildCmd succeeds
		// without error.
		program = nil
		// Build the program (piping its stdout and stderr to cmd.Stdout and
		// cmd.Stderr).
		buildCmd := exec.Command("go", buildArgs...)
		buildCmd.Env = cmd.Env
		buildCmd.Stdout = cmd.Stdout
		buildCmd.Stderr = cmd.Stderr
		err = buildCmd.Run()
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

func compileRegexps(patterns []string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	regexps := make([]*regexp.Regexp, len(patterns))
	buf := &bytes.Buffer{}
	var err error
	for i, pattern := range patterns {
		if strings.Count(pattern, ".") == 0 {
			regexps[i], err = regexp.Compile(buf.String())
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
		for _, r := range excludeDirRegexps {
			if r.MatchString(basename) {
				return filepath.SkipDir
			}
		}
		if len(dirRegexps) > 0 {
			matched := false
			for _, r := range dirRegexps {
				if r.MatchString(basename) {
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
