//go:build unix

package plugins

import "syscall"

// Opening a FIFO inside an owner-picked folder must never block a host call.
const folderReadFlags = syscall.O_RDONLY | syscall.O_NONBLOCK
