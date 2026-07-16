package harness

import (
	"fmt"
	"net"
	"os"
	"sync/atomic"
)

// portBase is the first port ever handed out. Every node reserves a stride of
// portStride ports so concurrent scenario processes stay in disjoint ranges
// even before any probing happens.
//
// pidSpread bounds how many stride-steps a process's PID folds into its
// starting offset (portBaseForProcess): two test binaries running
// concurrently (for example two packages under `go test ./...`) then start
// probing from different windows instead of the same base, cutting down how
// often probeUDP's bind-then-release race actually collides between them.
// This is a cheap mitigation, not a lock: two processes can still land on the
// same PID-derived offset (mod pidSpread) and race, same as before.
const (
	portBase   = 21000
	portStride = 4
	pidSpread  = 2000
)

// portCursor is the process-wide next-stride cursor. Starting at
// portBaseForProcess()-stride lets the first Add(portStride) return
// portBaseForProcess().
var portCursor atomic.Int32

func init() {
	portCursor.Store(int32(portBaseForProcess() - portStride))
}

// portBaseForProcess folds this OS process's PID into portBase, spreading
// concurrent test binaries into different starting windows.
func portBaseForProcess() int {
	return portBase + (os.Getpid()%pidSpread)*portStride
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
