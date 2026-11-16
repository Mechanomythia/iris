//go:build darwin

package peer

import "golang.org/x/sys/unix"

func peerPID(fd int) int {
	// SOL_LOCAL / LOCAL_PEERPID
	pid, err := unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	if err != nil {
		return 0
	}
	return pid
}
