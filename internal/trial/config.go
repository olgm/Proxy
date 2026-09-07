package trial

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

const (
	defaultHz       = 1.0
	defaultTimeout  = 2 * time.Second
	defaultMaxLogMB = 1024
	defaultLog      = "/var/lib/triald/trial.jsonl"
	defaultOverMS   = 5
	defaultCooldown = 10 * time.Minute
	defaultTraces   = 5
	defaultTraceBin = "mtr"
)

var defaultWindows = []string{"1m", "10m"}

// maxPath caps how far a probe may travel. The loop guard already stops a cycle;
// this stops a mesh somebody mis-wired from turning one probe into a flood that
// ends only when the datagram runs out of room to record where it has been.
const maxPath = 8

// Config is one node's part of the trial. Unlike probed's it is written by hand
// rather than derived from the routes, because every leg here is deliberately one
// no route uses — which is the whole question being asked. See agents/trial.md.
type Config struct {
	Name string `json:"name"`
	ID   uint8  `json:"id"`
	Bind string `json:"bind"`

	Legs []Leg `json:"legs"`
	Runs []Run `json:"runs,omitempty"`

	// Names is the whole mesh's name-to-id table, and every node gets all of it.
	// A node has to be able to spell a route it was never on: the exit sees copies
	// that came through nodes it has no leg to and starts no run from, and a path
	// written down as bare ids is not a dataset anyone reads. Optional, because a
	// node can name its own legs without it; where it disagrees with a leg, that
	// is a mesh wired two different ways and worth failing over.
	Names map[string]uint8 `json:"names,omitempty"`

	Hz        float64  `json:"hz,omitempty"`
	Windows   []string `json:"windows,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`

	Trace *Trace `json:"trace,omitempty"`

	Log      string `json:"log,omitempty"`
	MaxLogMB int    `json:"max_log_mb,omitempty"`
}

// Leg is one peer: where it is, the key that authenticates it, and what a round
// trip over it is expected to cost. ExpectMS is what the traceroute trigger reads
// and nothing else; it never changes what is sent or how it is measured.
type Leg struct {
	Peer     string  `json:"peer"`
	PeerID   uint8   `json:"peer_id"`
	Addr     string  `json:"addr"`
	Key      string  `json:"key"`
	ExpectMS float64 `json:"expect_ms"`

	// Echo makes this node send its own probe over this leg at the configured
	// rate. It is for a leg no run crosses: everywhere else a run's own traffic is
	// the leg's measurement, which is the point of building it this way.
	Echo bool `json:"echo,omitempty"`
}

// Run is one directed flood, named for the node that starts it. A node whose own
// name is From originates it; any other node listing it only says where to pass an
// arrival on. A node that does not list a run, or lists it with nowhere to
// forward, is that run's terminus.
//
// There is no role field here either, and for probed's reason: the links decide.
type Run struct {
	From    string   `json:"from"`
	FromID  uint8    `json:"from_id"`
	Forward []string `json:"forward,omitempty"`
}

// Trace is when to go and look at the path. A window landing more than OverMS
// above the leg's expectation is worth a traceroute; a leg that stays there is not
// worth one every window, which is what the cooldown is for.
type Trace struct {
	OverMS    float64 `json:"over_ms,omitempty"`
	CooldownS int     `json:"cooldown_s,omitempty"`
	Count     int     `json:"count,omitempty"`
	Bin       string  `json:"bin,omitempty"`
	TimeoutS  int     `json:"timeout_s,omitempty"`
}

// Validate reports whether triald would accept this config, without binding a
// socket or making a state directory. proxyctl checks what it generated before it
// uploads it: the alternative is finding out from six nodes that all failed to
// start at once. It takes a copy, so applying defaults cannot surprise the caller.
func Validate(c Config) error { return c.fill() }

func (c *Config) fill() error {
	if c.Name == "" {
		return fmt.Errorf("trial: name is required")
	}
	if c.ID == 0 {
		return fmt.Errorf("trial: %s: id must be 1-255, 0 is reserved", c.Name)
	}
	if c.Bind == "" {
		return fmt.Errorf("trial: %s: bind is required, because every node here is dialled by its peers", c.Name)
	}
	if len(c.Legs) == 0 {
		return fmt.Errorf("trial: %s: at least one leg", c.Name)
	}
	if c.Hz <= 0 {
		c.Hz = defaultHz
	}
	if len(c.Windows) == 0 {
		c.Windows = defaultWindows
	}
	if c.TimeoutMS == 0 {
		c.TimeoutMS = int(defaultTimeout / time.Millisecond)
	}
	if c.TimeoutMS < 0 {
		return fmt.Errorf("trial: %s: timeout_ms must not be negative", c.Name)
	}
	if c.MaxLogMB == 0 {
		c.MaxLogMB = defaultMaxLogMB
	}
	if c.MaxLogMB < 0 {
		return fmt.Errorf("trial: %s: max_log_mb must not be negative", c.Name)
	}
	if c.Log == "" {
		c.Log = defaultLog
	}

	// An id that means one node here and another node there would mislabel every
	// path either of them appears in, and nothing downstream could tell.
	for name, id := range c.Names {
		if id == 0 {
			return fmt.Errorf("trial: %s: names: %s has id 0, which is reserved", c.Name, name)
		}
		if name == c.Name && id != c.ID {
			return fmt.Errorf("trial: %s: names says we are id %d, config says %d", c.Name, id, c.ID)
		}
	}
	for _, l := range c.Legs {
		if id, ok := c.Names[l.Peer]; ok && id != l.PeerID {
			return fmt.Errorf("trial: %s: leg %s has id %d but names says %d", c.Name, l.Peer, l.PeerID, id)
		}
	}
	for _, r := range c.Runs {
		if id, ok := c.Names[r.From]; ok && id != r.FromID {
			return fmt.Errorf("trial: %s: run %s has id %d but names says %d", c.Name, r.From, r.FromID, id)
		}
	}

	seen := map[uint8]string{c.ID: c.Name}
	for i := range c.Legs {
		l := &c.Legs[i]
		if l.Peer == "" {
			return fmt.Errorf("trial: %s: leg %d has no peer name", c.Name, i)
		}
		if l.PeerID == 0 {
			return fmt.Errorf("trial: %s: leg %s: peer_id must be 1-255", c.Name, l.Peer)
		}
		if was, dup := seen[l.PeerID]; dup {
			return fmt.Errorf("trial: %s: id %d is both %s and %s", c.Name, l.PeerID, was, l.Peer)
		}
		seen[l.PeerID] = l.Peer
		if _, _, err := DecodeAddr(l.Addr); err != nil {
			return err
		}
		if l.ExpectMS < 0 {
			return fmt.Errorf("trial: %s: leg %s: expect_ms must not be negative", c.Name, l.Peer)
		}
	}

	runs := map[string]bool{}
	for _, r := range c.Runs {
		if r.From == "" {
			return fmt.Errorf("trial: %s: a run must name the node it starts from", c.Name)
		}
		if r.FromID == 0 {
			return fmt.Errorf("trial: %s: run %s: from_id must be 1-255", c.Name, r.From)
		}
		if r.From == c.Name && r.FromID != c.ID {
			return fmt.Errorf("trial: %s: run %s is our own but claims id %d, not %d", c.Name, r.From, r.FromID, c.ID)
		}
		if runs[r.From] {
			return fmt.Errorf("trial: %s: run %s is listed twice", c.Name, r.From)
		}
		runs[r.From] = true
		for _, p := range r.Forward {
			if c.leg(p) == nil {
				return fmt.Errorf("trial: %s: run %s forwards to %s, which is not a leg here", c.Name, r.From, p)
			}
		}
	}

	// A probe says which run it belongs to by whose id opens its path, so a node
	// that starts a run cannot also send bare echoes: its peers would forward
	// them as though they were that run. Echo exists for a leg no run crosses,
	// and on such a leg this never comes up.
	if c.originates() {
		for _, l := range c.Legs {
			if l.Echo {
				return fmt.Errorf("trial: %s: leg %s has echo set on a node that starts run %s; "+
					"its peers cannot tell the two apart", c.Name, l.Peer, c.Name)
			}
		}
	}

	if t := c.Trace; t != nil {
		if t.OverMS == 0 {
			t.OverMS = defaultOverMS
		}
		if t.CooldownS == 0 {
			t.CooldownS = int(defaultCooldown / time.Second)
		}
		if t.Count == 0 {
			t.Count = defaultTraces
		}
		if t.Bin == "" {
			t.Bin = defaultTraceBin
		}
		if t.TimeoutS == 0 {
			// A cycle is about a second, and a trace across thirty hops spends most
			// of each one waiting on the hops that never answer.
			t.TimeoutS = 30 + t.Count*2
		}
	}
	return nil
}

// originates reports whether any run listed here is this node's own. Like every
// other role in this repo it is read off the configuration rather than declared.
func (c *Config) originates() bool {
	for _, r := range c.Runs {
		if r.From == c.Name {
			return true
		}
	}
	return false
}

func (c *Config) leg(peer string) *Leg {
	for i := range c.Legs {
		if c.Legs[i].Peer == peer {
			return &c.Legs[i]
		}
	}
	return nil
}

// windows parses the configured window lengths. A window shorter than the probe
// interval would close on nothing.
func (c *Config) windows() ([]time.Duration, error) {
	interval := time.Duration(float64(time.Second) / c.Hz)
	out := make([]time.Duration, 0, len(c.Windows))
	for _, w := range c.Windows {
		d, err := time.ParseDuration(w)
		if err != nil {
			return nil, fmt.Errorf("trial: window %q: %w", w, err)
		}
		if d < interval {
			return nil, fmt.Errorf("trial: window %s is shorter than the %s probe interval", w, interval)
		}
		out = append(out, d)
	}
	return out, nil
}

// DecodeAddr splits a leg address. Every node in this mesh binds a known port and
// every peer dials it, so unlike probed's there is no bare-IP form: a leg with
// nowhere to send is a configuration error, not a peer we wait to hear from.
func DecodeAddr(addr string) (host string, port int, err error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("trial: address %q: want host:port: %w", addr, err)
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 || n > 65535 {
		return "", 0, fmt.Errorf("trial: address %q: bad port", addr)
	}
	return h, n, nil
}
