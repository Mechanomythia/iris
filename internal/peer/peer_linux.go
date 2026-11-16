//go:build linux

package peer

import "golang.org/x/sys/unix"

func peerPID(fd int) int {
	u, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0
	}
	return int(u.Pid)
}
