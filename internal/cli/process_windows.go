//go:build windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

// createNoWindow 让子进程不分配控制台（等价 CREATE_NO_WINDOW）。
const createNoWindow = 0x08000000

func startProcess(exe string, args []string, env []string, stdout, stderr *os.File) error {
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd.Start()
}
