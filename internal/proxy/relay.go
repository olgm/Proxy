package proxy

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olgm/proxy/internal/tunnel"
)

// relay copies in both directions until both are done. Each direction half-closes
// its own side on EOF rather than tearing down the whole connection, so a client
// that stops sending doesn't cut off data still in flight from the server.
// The byte counts it returns are payload: what the session carried, before the
// tunnel duplicated any of it.
//
// aErr and bErr are how each direction stopped: nil where the source reached a
// clean end of stream, and the read or write error where it did not. Discarding
// them is what left the exit unable to say whether a backend hung up or the path
// to it broke, so the exit logs them; see serveStream.
func relay(a, b halfCloser) (aToB, bToA int64, aErr, bErr error) {
	r := newRelayer(a, b)
	r.run()
	return r.ab.n, r.ba.n, r.ab.err, r.ba.err
}

// relayer is one relay, able to stop at a byte boundary for a handoff and to be
// started again from one. It copies through its own buffer rather than io.Copy:
// between two TCP connections io.Copy would reach splice(2), and an interrupted
// splice loses whatever it had already taken off the source, where this loop can
// say exactly which bytes it read and never wrote. A Minecraft session is far too
// light for the copy to matter.
type relayer struct {
	a, b   halfCloser
	halted atomic.Bool
	// ab is a to b, ba is b to a.
	ab, ba flow
	done   chan struct{}
}

// flow is one direction: what it has carried, and where it stopped.
type flow struct {
	n   int64
	err error
	// ended is set once the source is finished, cleanly or not; done once the
	// half-close that follows has been sent on. A halt can fall between the two.
	ended, done bool
	// halted says a handoff stopped this direction, and pending is what it had
	// read and not yet written when it did.
	halted  bool
	pending []byte
}

func newRelayer(a, b halfCloser) *relayer {
	return &relayer{a: a, b: b, done: make(chan struct{})}
}

// run copies both ways from wherever each direction was left, and returns when
// both are done or halted.
func (r *relayer) run() {
	defer close(r.done)
	var wg sync.WaitGroup
	for _, d := range []struct {
		dst, src halfCloser
		f        *flow
	}{{r.b, r.a, &r.ab}, {r.a, r.b, &r.ba}} {
		if d.f.done {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.pipe(d.dst, d.src, d.f)
		}()
	}
	wg.Wait()
}

func (r *relayer) pipe(dst, src halfCloser, f *flow) {
	f.halted = false
	if !f.ended {
		if !r.copy(dst, src, f) {
			return
		}
	}
	if err := dst.CloseWrite(); err != nil && r.halting(err) {
		f.halted = true
		return
	}
	f.done = true
}

// copy moves bytes until the source ends, and reports false if a halt stopped it
// first.
func (r *relayer) copy(dst, src halfCloser, f *flow) bool {
	if len(f.pending) > 0 {
		n, err := dst.Write(f.pending)
		f.n += int64(n)
		f.pending = f.pending[n:]
		if err != nil {
			return r.stop(f, err)
		}
		f.pending = nil
	}
	buf := make([]byte, 32<<10)
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			f.n += int64(nw)
			if werr != nil {
				f.pending = append([]byte(nil), buf[nw:nr]...)
				return r.stop(f, werr)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				rerr = nil
			}
			return r.stop(f, rerr)
		}
	}
}

// stop settles a direction on err: halted if that is what the error is, ended
// otherwise, and nil is a clean end. Reports whether the direction ended.
func (r *relayer) stop(f *flow, err error) bool {
	if err != nil && r.halting(err) {
		f.halted = true
		return false
	}
	f.pending = nil
	f.ended, f.err = true, err
	return true
}

// halting tells a halt apart from a real failure that happened to come at the
// same moment. A connection reset is still a reset.
func (r *relayer) halting(err error) bool {
	return r.halted.Load() && (errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, tunnel.ErrHalted))
}

// halt stops both directions where they stand and waits for them. A TCP end is
// stopped by a deadline, which is local to this process and never reaches the
// socket; a tunnel stream by its own Halt.
func (r *relayer) halt() {
	r.halted.Store(true)
	for _, e := range []halfCloser{r.a, r.b} {
		switch c := e.(type) {
		case *net.TCPConn:
			c.SetDeadline(time.Now())
		case *tunnel.Stream:
			c.Halt()
		}
	}
}

// carried reports whether a halt left anything for the next process to do. A
// relay that finished on its own in the same moment has nothing to hand over.
func (r *relayer) carried() bool {
	return (r.ab.halted || r.ba.halted) && !(r.ab.done && r.ba.done)
}
