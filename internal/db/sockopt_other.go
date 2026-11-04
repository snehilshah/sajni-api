//go:build !linux

package db

import "syscall"

// tcpUserTimeout is Linux-only; elsewhere keepalives and statement_timeout
// still apply.
func tcpUserTimeout(_, _ string, _ syscall.RawConn) error { return nil }
