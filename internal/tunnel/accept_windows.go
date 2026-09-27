//go:build windows

package tunnel

// forwardConnCeiling returns the number of descriptors the count of forwarded
// connections is drawn from. Windows has no RLIMIT_NOFILE: a socket is a
// handle, and handles run out on memory rather than on a limit of the process
// that could be read. This is the soft limit the process runs with on other
// systems once main has raised it (checkUlimit), so a forward carries as many
// connections here as it does there.
func forwardConnCeiling() int64 {
	return 65535
}
