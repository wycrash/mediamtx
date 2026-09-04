//go:build !windows

package core

import (
	"os"
	"syscall"
)

func reexec() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(executable, os.Args, os.Environ())
}
