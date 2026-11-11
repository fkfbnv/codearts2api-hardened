//go:build !windows

package cli

import (
	"os"
	"os/exec"
)

func startProcess(exe string, args []string, env []string, stdout, stderr *os.File) error {
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Start()
}
