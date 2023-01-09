package wgo

import (
	"os/exec"
	"strconv"
)

func stop(program *exec.Cmd) {
	if program.Process == nil {
		return
	}
	// https://stackoverflow.com/a/44551450 
	exec.Command("TASKKILL", "/T", "/F", "/PID", strconv.Itoa(program.Process.Pid)).Run()
}
