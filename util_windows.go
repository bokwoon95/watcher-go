package wgo

import (
	"io"
	"os/exec"
	"strconv"
)

func kill(runCmd *exec.Cmd) error {
	// https://stackoverflow.com/a/44551450
	kill := exec.Command("TASKKILL", "/T", "/F", "/PID", strconv.Itoa(runCmd.Process.Pid))
	return kill.Run()
}

func newRunCmd(name string, args ...string) (runCmd *exec.Cmd, stdout, stderr io.ReadCloser, err error) {
	runCmd = exec.Command(name, args...)
	stdout, err = runCmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err = runCmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	err = runCmd.Start()
	if err != nil {
		return nil, nil, nil, err
	}
	return runCmd, stdout, stderr, err
}
