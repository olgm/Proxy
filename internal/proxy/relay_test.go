package proxy

import (
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
)

// tcpPair is two ends of one real TCP connection. net.Pipe will not do here: the
// whole point is the distinction between a clean FIN and an RST, which only a
// kernel socket makes.
func tcpPair(t *testing.T) (client, server *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan *net.TCPConn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- nil
			return
		}
		done <- c.(*net.TCPConn)
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s := <-done
	if s == nil {
		t.Fatal("accept failed")
	}
	return c.(*net.TCPConn), s
}

// A backend that hangs up hard and one that closes cleanly have to be two
// different things in the log, because they are two different incidents: one is
// the far end ending a session, the other is the path to it breaking. Before the
// exit recorded relay's errors they were indistinguishable, and a fleet-wide drop
// could not be charged to anyone.
func TestRelayReportsHowTheBackendEnded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset bool
	}{
		{"clean close is an eof", false},
		{"reset is an error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, far := tcpPair(t) // backend is the exit's side of the dial
			chain, peer := tcpPair(t)  // chain stands in for the tunnel stream

			out := make(chan error, 1)
			go func() {
				_, _, _, backendErr := relay(chain, backend)
				out <- backendErr
			}()

			if tc.reset {
				// Linger zero turns Close into an RST rather than a FIN.
				if err := far.SetLinger(0); err != nil {
					t.Fatal(err)
				}
			}
			far.Close()
			// Let the other direction finish so relay can return.
			peer.Close()

			backendErr := <-out
			switch {
			case tc.reset && backendErr == nil:
				t.Fatal("a reset backend reported a clean end of stream")
			case tc.reset && !errors.Is(backendErr, syscall.ECONNRESET):
				t.Logf("reset reported as %v", backendErr)
			case !tc.reset && backendErr != nil && !errors.Is(backendErr, io.EOF):
				t.Fatalf("a clean close reported %v", backendErr)
			}
			if got := ended(backendErr); tc.reset == (got == "eof") {
				t.Fatalf("ended(%v) = %q, which does not tell the two apart", backendErr, got)
			}
		})
	}
}
