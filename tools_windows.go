//go:build windows

package main

import "os/exec"

func setupProcessGroup(cmd *exec.Cmd) {
	// Windows doesn't support Unix process groups; Go's default
	// exec.CommandContext cancellation (taskkill) is sufficient.
}
