//go:build !windows

package imagedec

import "os/exec"

func hideConsoleWindow(cmd *exec.Cmd) {}
