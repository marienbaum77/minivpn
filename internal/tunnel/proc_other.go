//go:build !windows

package tunnel

import "os/exec"

func hideWindow(*exec.Cmd) {}

func killWithParent(*exec.Cmd) error { return nil }
