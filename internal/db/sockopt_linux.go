package db

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// tcpUserTimeout closes a connection whose sent data goes unacknowledged for
// tcpUserMs, instead of the kernel's ~15 minutes of retries.
func tcpUserTimeout(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, tcpUserMs)
	}); err != nil {
		return err
	}
	return serr
}
