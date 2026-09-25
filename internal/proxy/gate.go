package proxy

import (
	"net"
	"sync"
	"time"
)

// gate is what a handoff closes. It knows every connection this node is working
// on: the ones still being admitted — a handshake, a whitelist check, a dial — and
// the ones relaying. A handoff shuts the doors, gives the first kind a moment to
// become the second, and then halts every relay where it stands.
type gate struct {
	mu     sync.Mutex
	shut   bool
	frozen bool
	busy   map[*admission]struct{}
	relays map[*relayer]*carry
	// released is closed once a handoff has put everything it carries in the
	// store. A halted relay's goroutine waits on it and then simply returns:
	// the session is not over, so nothing about it is written down here.
	released chan struct{}
}

// admission is one connection on its way to becoming a relay. c is the player's
// or the previous hop's connection, when there is one to cut off.
type admission struct{ c *net.TCPConn }

// carry is what a handoff writes down about a relay besides its two ends and its
// bytes: which listener it belongs to, and, for a login, the session.
type carry struct {
	bind  string
	net   string
	sess  *Session
	rtt   *clientRTT
	start time.Time
}

func newGate() *gate {
	return &gate{busy: map[*admission]struct{}{}, relays: map[*relayer]*carry{}, released: make(chan struct{})}
}

// enter admits a connection, or refuses it with nil once a handoff has frozen the
// node: it is left for the next process, or closed as one no process will carry.
func (g *gate) enter(c *net.TCPConn) *admission {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.frozen {
		return nil
	}
	a := &admission{c: c}
	g.busy[a] = struct{}{}
	return a
}

// leave is deferred by whatever entered. After start it does nothing.
func (g *gate) leave(a *admission) {
	g.mu.Lock()
	delete(g.busy, a)
	g.mu.Unlock()
}

// start turns an admission into a relay, and reports false if a handoff froze the
// node first — in which case the caller gives the connection up without relaying
// a byte. Called before anything the relay starts has side effects.
func (g *gate) start(a *admission, r *relayer, k *carry) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.busy, a)
	if g.frozen {
		return false
	}
	g.relays[r] = k
	return true
}

// adopt registers a relay a handoff carried in, which was admitted long ago. It
// is called before the node is ready, while nothing can be freezing the gate.
func (g *gate) adopt(r *relayer, k *carry) {
	g.mu.Lock()
	g.relays[r] = k
	g.mu.Unlock()
}

// end forgets a relay that finished on its own.
func (g *gate) end(r *relayer) {
	g.mu.Lock()
	delete(g.relays, r)
	g.mu.Unlock()
}

// park is where a relay's goroutine waits once a handoff has halted it.
func (g *gate) park() { <-g.released }

func (g *gate) isShut() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.shut
}

// isFrozen reports whether a handoff has frozen the node, which is also when it
// cuts off every connection still being admitted.
func (g *gate) isFrozen() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.frozen
}

func (g *gate) shutDoors() {
	g.mu.Lock()
	g.shut = true
	g.mu.Unlock()
}

// settle waits for every admission to become a relay or give up, for at most d.
func (g *gate) settle(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		n := len(g.busy)
		g.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// freeze stops any more relays starting and returns the ones running, with the
// connections still being admitted, whose reads the caller cuts short.
func (g *gate) freeze() (map[*relayer]*carry, []*net.TCPConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.frozen = true
	relays := make(map[*relayer]*carry, len(g.relays))
	for r, k := range g.relays {
		relays[r] = k
	}
	var cut []*net.TCPConn
	for a := range g.busy {
		if a.c != nil {
			cut = append(cut, a.c)
		}
	}
	return relays, cut
}

// door is a listener a handoff can shut without closing. Accept stops and reports
// the listener closed, while the socket and its backlog stay as they are for the
// next process: a player connecting in between waits in the kernel's queue, not
// on a refused connection.
type door struct {
	*net.TCPListener
	g *gate
}

func (d door) Accept() (net.Conn, error) {
	c, err := d.TCPListener.Accept()
	if err != nil && d.g.isShut() {
		return nil, net.ErrClosed
	}
	return c, err
}
