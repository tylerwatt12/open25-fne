//go:build !linux

package dispatch

import "syscall"

func codecProcessAttributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
