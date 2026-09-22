package proxy

import (
	"io"
	"testing"
)

// nothing is a halfCloser that is not a tunnel stream, which is what a direct
// exit hands the relay.
type nothing struct{}

func (nothing) Read([]byte) (int, error)    { return 0, io.EOF }
func (nothing) Write(b []byte) (int, error) { return len(b), nil }
func (nothing) CloseWrite() error           { return nil }
func (nothing) Close() error                { return nil }

// A session on a direct exit costs both ends of it and nothing else: the node is
// billed for what it takes from the player and sends to the backend, and for what
// comes back the other way. There is no tunnel to duplicate anything, so the
// answer is exactly twice the payload — which is also the floor for every other
// shape, and the reason the figure is worth showing on a route with no tunnel at
// all.
func TestChainCostOfADirectExitIsBothEndsOfIt(t *testing.T) {
	const up, down = 4200, 51700
	if got := chainCost(nothing{}, 0, up, down); got != 2*(up+down) {
		t.Errorf("chainCost = %d, want %d", got, 2*(up+down))
	}
	// A hop count on a route with no tunnel is not a reason to invent traffic.
	if got := chainCost(nothing{}, 3, up, down); got != 2*(up+down) {
		t.Errorf("chainCost with legs but no tunnel = %d, want %d", got, 2*(up+down))
	}
	if got := chainCost(nothing{}, 0, 0, 0); got != 0 {
		t.Errorf("a session that carried nothing cost %d", got)
	}
}
