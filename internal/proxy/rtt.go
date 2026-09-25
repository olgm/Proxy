package proxy

import (
	"fmt"
	"math"
	"net"
	"slices"
	"time"

	"github.com/olgm/proxy/internal/control"
)

// rttEvery is how often a session's client leg is read. The kernel's figure is
// already smoothed over every acknowledgement, so this samples a running number
// rather than timing anything itself.
const rttEvery = 5 * time.Second

// tcpSample is one reading of a connection's round trip, as tcpInfo decodes it.
type tcpSample struct {
	rtt, rttvar, minRTT time.Duration
	retrans             uint32
}

// clientRTT reads a player's connection for as long as the session lasts. That
// leg — the player to this entry — is the one part of their ping the fleet does
// not otherwise see: probed and the tunnel time every leg after the entry, never
// the one before it.
type clientRTT struct {
	c    *net.TCPConn
	stop chan struct{}
	done chan struct{}

	rtt, rttvar []time.Duration
	min         time.Duration
	retrans     uint32
}

func watchRTT(c *net.TCPConn) *clientRTT {
	return resumeRTT(c, rttState{})
}

// rttState is a session's client-leg readings so far, carried by a handoff so the
// summary at logout covers the whole session and not only its last process.
type rttState struct {
	RTT     []time.Duration `json:"rtt,omitempty"`
	RTTVar  []time.Duration `json:"rttvar,omitempty"`
	Min     time.Duration   `json:"min,omitempty"`
	Retrans uint32          `json:"retrans,omitempty"`
}

func resumeRTT(c *net.TCPConn, st rttState) *clientRTT {
	w := &clientRTT{c: c, stop: make(chan struct{}), done: make(chan struct{}),
		rtt: st.RTT, rttvar: st.RTTVar, min: st.Min, retrans: st.Retrans}
	go w.run()
	return w
}

// halt stops the sampling without a last reading and hands back what it has.
func (w *clientRTT) halt() rttState {
	close(w.stop)
	<-w.done
	return rttState{RTT: w.rtt, RTTVar: w.rttvar, Min: w.min, Retrans: w.retrans}
}

func (w *clientRTT) run() {
	defer close(w.done)
	t := time.NewTicker(rttEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			w.sample()
		case <-w.stop:
			return
		}
	}
}

func (w *clientRTT) sample() {
	s, ok := tcpInfo(w.c)
	if !ok || s.rtt == 0 {
		return
	}
	w.rtt = append(w.rtt, s.rtt)
	w.rttvar = append(w.rttvar, s.rttvar)
	if s.minRTT > 0 && (w.min == 0 || s.minRTT < w.min) {
		w.min = s.minRTT
	}
	w.retrans = s.retrans
}

// end stops the sampling, takes one last reading and sums the session up. It runs
// once the relay has returned and before handle closes the socket, so the last
// reading is still there to take — and it is the only one a session shorter than
// rttEvery gets. Nil when nothing could be read at all.
func (w *clientRTT) end() *control.RTT {
	close(w.stop)
	<-w.done
	w.sample()
	return w.summary()
}

func (w *clientRTT) summary() *control.RTT {
	if len(w.rtt) == 0 {
		return nil
	}
	rtt := slices.Sorted(slices.Values(w.rtt))
	rttvar := slices.Sorted(slices.Values(w.rttvar))
	return &control.RTT{
		Min:     ms(w.min),
		P50:     ms(pct(rtt, 50)),
		P90:     ms(pct(rtt, 90)),
		Max:     ms(rtt[len(rtt)-1]),
		Var:     ms(pct(rttvar, 50)),
		Retrans: w.retrans,
		N:       len(rtt),
	}
}

// pct indexes sorted samples the way internal/window and tools/tcpping do, so a
// figure here can be put beside probed's.
func pct(sorted []time.Duration, p int) time.Duration {
	return sorted[p*(len(sorted)-1)/100]
}

// ms is milliseconds to one decimal, as probed and the link line report them.
func ms(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Millisecond)*10) / 10
}

// rttPart is the logout line's account of the client leg: min/p50/p90/max, then
// the deviation and the re-sends. Empty when there is none, rather than zeros
// that would read as a player sitting on top of the node.
func rttPart(r *control.RTT) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf(" rtt=%.1f/%.1f/%.1f/%.1fms rttvar=%.1fms retrans=%d",
		r.Min, r.P50, r.P90, r.Max, r.Var, r.Retrans)
}
