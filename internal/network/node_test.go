package network

import "testing"

func TestConnectedPeersCountsPeerMap(t *testing.T) {
	n := &Node{peers: map[string]*Peer{"a": {}, "b": {}}}
	if got := n.ConnectedPeers(); got != 2 {
		t.Fatalf("ConnectedPeers = %d, want 2", got)
	}
}
