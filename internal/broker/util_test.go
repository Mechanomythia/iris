package broker

import "net"

func netDial(p string) (net.Conn, error) { return net.Dial("unix", p) }
