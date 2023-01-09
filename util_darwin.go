package wgo

import (
	"os/exec"
	"syscall"
)

func stop(program *exec.Cmd) {
	// https://stackoverflow.com/questions/22470193/why-wont-go-kill-a-child-process-correctly
	_ = syscall.Kill(-program.Process.Pid, syscall.SIGKILL)
	// Wait releases any resources associated with the Process.
	_, _ = program.Process.Wait()
}
