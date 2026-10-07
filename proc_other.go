//go:build !unix

package main

import "os/exec"

// killGroup: on other systems only the main process is stopped.
func killGroup(cmd *exec.Cmd) {}
