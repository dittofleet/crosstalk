//go:build !windows

package daemon

// shortTemp is where a test's socket goes: a socket path has little room,
// and the usual temp directory is long on macOS.
const shortTemp = "/tmp"
