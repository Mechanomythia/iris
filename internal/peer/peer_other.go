//go:build !darwin && !linux

package peer

func peerPID(int) int { return 0 }
