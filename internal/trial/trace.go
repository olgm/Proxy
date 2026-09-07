package trial

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"

	"github.com/olgm/proxy/internal/window"
)

// tracer decides when a leg is worth looking at and rate-limits the looking.
//
// The cooldown is per leg rather than per node. A node with four legs that all
// degrade at once has four different paths to describe, and one cooldown across
// the node would describe whichever window happened to close first and stay quiet
// about the rest — which is the case where the answer matters most.
type tracer struct {
	cfg Trace
	now func() time.Time
	// run is the seam a test replaces, so the tests neither shell out nor need a
	// network to reach.
	run func(ctx context.Context, bin string, args ...string) ([]byte, error)

	mu   sync.Mutex
	last map[string]time.Time
}

func newTracer(cfg Trace) *tracer {
	return &tracer{
		cfg:  cfg,
		now:  time.Now,
		run:  runCommand,
		last: map[string]time.Time{},
	}
}

func runCommand(ctx context.Context, bin string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, bin, args...).Output()
}

// take reports whether this leg may be traced now, and records that it was. The
// cooldown starts when a trace is authorised rather than when it finishes, so one
// that hangs until its timeout cannot be followed immediately by another.
func (t *tracer) take(peer string) bool {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, seen := t.last[peer]; seen && now.Sub(last) < time.Duration(t.cfg.CooldownS)*time.Second {
		return false
	}
	t.last[peer] = now
	return true
}

// trace walks the path to one peer.
//
// UDP to the port the probe itself uses, so the five-tuple hashes onto whichever
// ECMP path the probe takes. An ICMP trace describes a path our traffic may never
// see: on this chain ICMP and TCP have measured 2.9 ms apart over the same leg,
// and the error is path-dependent, so it cannot be corrected for afterwards.
//
// mtr's own JSON is returned unparsed. Reading it here would be one more thing to
// keep current with a tool we do not own, and the raw document is what an operator
// would want in front of them anyway.
func (t *tracer) trace(l *leg) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(t.cfg.TimeoutS)*time.Second)
	defer cancel()

	out, err := t.run(ctx, t.cfg.Bin,
		"--json", "--udp", "-n",
		"-c", fmt.Sprint(t.cfg.Count),
		"-P", fmt.Sprint(l.addr.Port),
		l.ip.String())
	if err != nil {
		return nil, err
	}
	if !json.Valid(out) {
		return nil, fmt.Errorf("trial: %s returned no usable json", t.cfg.Bin)
	}
	return json.RawMessage(out), nil
}

// consider is the trigger. A window landing more than the configured margin above
// what this leg is supposed to cost is worth a look at the path.
//
// It reads a closed window rather than a single probe on purpose. At one probe a
// second, a leg whose mdev is a few milliseconds would trip a per-sample trigger
// more or less continuously, and a path that was slow for one packet is not a path
// worth describing. A window that has closed above expectation is.
func (n *Node) consider(l *leg, c *window.Closed) {
	if n.tr == nil || c.N == 0 || l.expect <= 0 {
		return
	}
	if c.P50 <= l.expect+n.tr.cfg.OverMS {
		return
	}
	if !n.tr.take(l.peer) {
		return
	}

	// Off the measurement's own goroutine: a trace takes seconds and the windows
	// behind it must keep closing on time. Tracked by the node's WaitGroup so a
	// shutdown waits for the record rather than losing it.
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		hops, err := n.tr.trace(l)
		rec := traced{
			T: n.tr.now().UTC().Format(time.RFC3339), K: "trace", W: dur(c.Window),
			Leg: l.name, P50: round(c.P50), Expect: l.expect, Hops: hops,
		}
		if err != nil {
			rec.Err = err.Error()
		}
		log.Printf("%s: trial %s p50=%.1fms over expect=%.1fms, traced", n.cfg.Name, l.name, c.P50, l.expect)
		if err := n.w.Write(rec); err != nil {
			log.Printf("%s: trial log: %v", n.cfg.Name, err)
		}
	}()
}
