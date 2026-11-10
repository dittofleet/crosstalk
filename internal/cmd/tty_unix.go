//go:build !windows

package cmd

// The terminal itself, which stdin may not be.
const ttyIn, ttyOut = "/dev/tty", "/dev/tty"
