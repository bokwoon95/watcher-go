package wgo

func kill(runCmd *exec.Cmd) error {
	// https://stackoverflow.com/questions/22470193/why-wont-go-kill-a-child-process-correctly
	err = syscall.Kill(-runCmd.Process.Pid, syscall.SIGKILL)
	// Wait releases any resources associated with the Process.
	_, _ = runCmd.Process.Wait()
	return
}
