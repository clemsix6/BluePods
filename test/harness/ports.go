package harness

import (
	"fmt"
	"net"
	"sync/atomic"
)

// portBase is the first port ever handed out. Every node reserves a stride of
// portStride ports so concurrent scenario processes stay in disjoint ranges
// even before any probing happens.
const (
	portBase   = 21000
	portStride = 4
)

// portCursor is the process-wide next-stride cursor. Starting at portBase-stride
// lets the first Add(portStride) return portBase.
var portCursor atomic.Int32

func init() {
	portCursor.Store(portBase - portStride)
}

// allocatePort reserves the next port stride and returns its first port,
// probing it by binding a UDP socket (the node's QUIC transport) and
// releasing it immediately. A bound port is skipped and the next stride is
// tried, so two concurrent test binaries never collide even though both
// count from roughly the same base.
func allocatePort() (int, error) {
	for attempts := 0; attempts < 1000; attempts++ {
		port := int(portCursor.Add(portStride))

		if probeUDP(port) {
			return port, nil
		}
	}

	return 0, fmt.Errorf("could not find a free port after 1000 attempts")
}

// probeUDP reports whether port is free by binding to it and releasing it.
func probeUDP(port int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return false
	}

	conn.Close()

	return true
}
