//go:build windows

package imagedec

import (
	"os/exec"
	"syscall"
)

// hideConsoleWindow: children don't flash a console when f4 has none.
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
