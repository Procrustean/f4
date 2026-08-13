//go:build !windows

package imagedecoders

import "os/exec"

func hideConsoleWindow(cmd *exec.Cmd) {}
