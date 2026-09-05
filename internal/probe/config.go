package probe

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// Defaults. The rate is per class per second, so the traffic a node puts on a leg
// is Hz × copies × 2, and every one of those datagrams is under 100 bytes on the
// wire. Two windows are kept because they answer different questions: a short one
// is what a graph needs to show an incident while it is happening, and a long one
// is the only way a percentile out at p99 has enough samples behind it to mean
// anything. Both are written; neither is derived from the other.
const (
	defaultHz       = 1.0
	defaultTimeout  = 2 * time.Second
	defaultMaxLogMB = 256
	defaultLog      = "/var/lib/probed/probe.jsonl"
)

var defaultWindows = []string{"1m", "10m"}

// Config is one node's whole job. proxyctl writes it to /etc/probed/config.json
// from the same topology proxyd's own config comes from, so the legs probed are
// the legs deployed and cannot drift from them.
type Config struct {
	Name string `json:"name"`
	// Bind is the address peers reach us at. Empty is valid, and is what a node
	// that only originates gets: it dials out and answers come back on the same
	// socket, so it needs no inbound rule of its own.
	Bind string `json:"bind,omitempty"`
	// Links are every leg this node touches, in no particular order. Classes name
	// them by index. Addr is host:port for a leg we dial and a bare IP for one
	// that dials us, the same convention proxyd's own config uses and for the same
	// reason: a node that dials uses an ephemeral source port, so there is none to
	// match on.
	Links   []LinkConfig `json:"links"`
	Classes []Class      `json:"classes"`

	// Hz is probes per second, per class. Fractional is allowed.
	Hz float64 `json:"hz,omitempty"`
	// Windows are the aggregation periods, as Go durations. Every one of them is
	// written to the log for every class.
	Windows []string `json:"windows,omitempty"`
	// TimeoutMS is how long an unanswered probe waits before it is called lost.
	// It is also how long a window's flush is held back past its own end, so that
	// a probe still in flight when the window closed is counted in the window it
	// was sent in rather than written off. proxyd learned that one the hard way:
	// counting sends and replies in separate windows reported a steady 3% loss on
	// a chain that was dropping nothing.
	TimeoutMS int `json:"timeout_ms,omitempty"`

	Log string `json:"log,omitempty"`
	// MaxLogMB rotates the log at this size, keeping one previous file. Disk is
	// therefore bounded at twice this and never grows without limit.
	MaxLogMB int `json:"max_log_mb,omitempty"`
}

// LinkConfig is one leg: where it goes and the key it is sealed with. The key is
// the leg's own and is not proxyd's — probed compromised must not be a way into a
// live session, and it runs as its own user for the same reason.
type LinkConfig struct {
	Addr string `json:"addr"`
	Key  string `json:"key"`
}

// Class is one measurement: a set of links, a number of copies to put on them,
// and a name to report it under. Two classes over the same leg differing only in
// Duplicate are the whole point — the difference between them is what duplication
// buys on that leg.
//
// What a node does with a class follows from its links, exactly as it does in the
// tunnel. Down but no up makes this node the originator, up but no down the
// responder, both a relay. There is no role field on purpose.
type Class struct {
	// ID identifies the class on the wire and must agree on every node carrying
	// it. Zero is not a valid id, so a truncated datagram cannot name a class.
	ID uint8 `json:"id"`
	// Name is how the class is reported: "hk>ty" for a leg, "hk>ty>ch" for a
	// chain. Kind is "leg" or "chain" and is for reporting only — nothing in the
	// protocol branches on it.
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Duplicate is how many copies of every probe this node puts on each of its
	// links for this class. It applies to what this node sends, whether it
	// originated the probe, is forwarding it, or is answering it — so a relay
	// receiving two copies still sends the count of the leg after, and the counts
	// never compound along a chain.
	Duplicate int `json:"duplicate"`
	// Up and Down are the legs this class crosses: Down toward the responder, Up
	// toward the originator. More than one Down is a race, and the first answer
	// back wins, which is the same thing the data plane does with a chunk.
	Up   []Hop `json:"up,omitempty"`
	Down []Hop `json:"down,omitempty"`
}

// Hop is one link a class uses, and how many copies to put on it. The count lives
// here rather than on the class because two paths into the same exit may carry
// different numbers of copies, and a node racing both has to honour each — the
// same reason the tunnel hangs its count off a Link.
type Hop struct {
	Link int `json:"link"`
	// Duplicate overrides the class default for this leg. Zero takes the default.
	Duplicate int `json:"duplicate,omitempty"`
}

// dup is how many copies this hop carries, falling back to the class default.
func (c Class) dup(h Hop) int {
	if h.Duplicate > 0 {
		return h.Duplicate
	}
	return c.Duplicate
}

const (
	KindLeg   = "leg"
	KindChain = "chain"
)

func (c *Config) fill() error {
	if c.Hz <= 0 {
		c.Hz = defaultHz
	}
	if len(c.Windows) == 0 {
		c.Windows = defaultWindows
	}
	if c.TimeoutMS == 0 {
		c.TimeoutMS = int(defaultTimeout / time.Millisecond)
	}
	if c.MaxLogMB == 0 {
		c.MaxLogMB = defaultMaxLogMB
	}
	if c.Log == "" {
		c.Log = defaultLog
	}
	if c.TimeoutMS < 0 || c.MaxLogMB < 0 {
		return errors.New("probe: timeout_ms and max_log_mb cannot be negative")
	}
	if len(c.Classes) == 0 {
		return errors.New("probe: a node with no classes has nothing to measure")
	}
	seen := map[uint8]bool{}
	for i := range c.Classes {
		cl := &c.Classes[i]
		if cl.ID == 0 {
			return errors.New("probe: class id 0 is reserved")
		}
		if seen[cl.ID] {
			return fmt.Errorf("probe: class id %d is listed twice", cl.ID)
		}
		seen[cl.ID] = true
		if cl.Duplicate < 1 {
			cl.Duplicate = 1
		}
		if len(cl.Up) == 0 && len(cl.Down) == 0 {
			return fmt.Errorf("probe: class %q reaches nothing", cl.Name)
		}
		for _, h := range append(append([]Hop{}, cl.Up...), cl.Down...) {
			if h.Link < 0 || h.Link >= len(c.Links) {
				return fmt.Errorf("probe: class %q names link %d of %d", cl.Name, h.Link, len(c.Links))
			}
			if h.Duplicate < 0 {
				return fmt.Errorf("probe: class %q asks for %d copies", cl.Name, h.Duplicate)
			}
		}
	}
	for _, l := range c.Links {
		if _, _, err := DecodeAddr(l.Addr); err != nil {
			return err
		}
	}
	if c.Bind == "" {
		// Only a node that answers or forwards needs somewhere to be reached at.
		for _, cl := range c.Classes {
			if len(cl.Up) > 0 {
				return fmt.Errorf("probe: class %q is answered here but there is no bind address", cl.Name)
			}
		}
	}
	return nil
}

// windows parses the aggregation periods, rejecting one shorter than the probe
// interval — a window that cannot hold a single probe reports nothing but gaps.
func (c *Config) windows() ([]time.Duration, error) {
	interval := time.Duration(float64(time.Second) / c.Hz)
	out := make([]time.Duration, 0, len(c.Windows))
	for _, s := range c.Windows {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("probe: window %q: %w", s, err)
		}
		if d < interval {
			return nil, fmt.Errorf("probe: window %s is shorter than the %v probe interval", s, interval)
		}
		out = append(out, d)
	}
	return out, nil
}

// DecodeAddr splits a link address into a host and, for a leg we dial, a port.
// A bare IP means the far end dials us, so its source port is ephemeral and is
// not matched on.
func DecodeAddr(addr string) (host string, port int, err error) {
	h, p, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		if net.ParseIP(addr) == nil {
			return "", 0, fmt.Errorf("probe: link address %q is neither an IP nor host:port", addr)
		}
		return addr, 0, nil
	}
	n, convErr := net.LookupPort("udp", p)
	if convErr != nil {
		return "", 0, fmt.Errorf("probe: link address %q: %w", addr, convErr)
	}
	return h, n, nil
}
