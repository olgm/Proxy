package trial

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/olgm/proxy/internal/window"
)

// arrival is one probe landing here. Every copy is written and nothing is
// collapsed to one, which is the whole difference from probed: two copies of the
// same tick arriving over different paths *is* the measurement, and it is one
// clock reading both of them, so the gap between them means something where a
// difference taken across two nodes would not.
//
// The timestamp is nanosecond-resolution on purpose. Copies racing into Tokyo
// land within a millisecond of each other and a second-resolution stamp would
// report every one of those races as a tie.
type arrival struct {
	T      string `json:"t"`
	K      string `json:"k"`
	Leg    string `json:"leg"`  // peer>us: the direction this datagram travelled
	Tick   uint64 `json:"tick"` // the second its origin sent it; the cross-node join key
	Path   string `json:"path"` // the whole route, origin first and us last
	Seq    uint64 `json:"lseq"`
	Onward int    `json:"onward"` // copies passed on from here; 0 at a terminus
}

// closed is one leg-direction over one window. The fields probed writes are
// spelled the same way here so the two datasets can be read side by side, which
// is the only way the candidate's numbers can be set against the incumbent's.
type closed struct {
	T    string  `json:"t"`
	K    string  `json:"k"`
	W    string  `json:"w"`
	Leg  string  `json:"leg"` // us>peer: the direction we sent in
	N    int     `json:"n"`
	Sent int     `json:"sent"`
	Got  int     `json:"got"`
	Fwd  *int    `json:"fwd"`
	Loss float64 `json:"loss"`
	// LossFwd and LossRev split the round trip into the direction that dropped.
	// Null rather than zero when the peer's counters could not be read across this
	// window: no echo arrived, so nothing is known about either direction beyond
	// the fact that the round trip failed.
	LossFwd *float64 `json:"loss_fwd"`
	LossRev *float64 `json:"loss_rev"`

	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	Mdev float64 `json:"mdev"`

	// Expect is what this leg was configured to cost. It rides along so a reader
	// can see what the trigger was comparing against without holding the config.
	Expect float64 `json:"expect"`
}

// traced is one look at the path and the window that prompted it. The hops are
// mtr's own JSON, stored as it came: parsing it here would be one more thing to
// keep current with a tool we do not own.
type traced struct {
	T      string          `json:"t"`
	K      string          `json:"k"`
	W      string          `json:"w"`
	Leg    string          `json:"leg"`
	P50    float64         `json:"p50"`
	Expect float64         `json:"expect"`
	Hops   json.RawMessage `json:"hops,omitempty"`
	Err    string          `json:"err,omitempty"`
}

func (n *Node) recordArrival(at time.Time, legName string, p packet, path []uint8, onward int) arrival {
	return arrival{
		T: at.UTC().Format(time.RFC3339Nano), K: "rx",
		Leg: legName, Tick: p.tick, Path: n.pathName(path), Seq: p.legSeq, Onward: onward,
	}
}

func record(legName string, expect float64, c *window.Closed) closed {
	fwd, rev, rt := c.Loss()
	l := closed{
		T: c.At.Format(time.RFC3339), K: "win", W: dur(c.Window), Leg: legName,
		N: c.N, Sent: c.Sent, Got: c.Got, Loss: round(rt),
		Min: round(c.Min), Max: round(c.Max), Mean: round(c.Mean),
		P50: round(c.P50), P90: round(c.P90), P99: round(c.P99), Mdev: round(c.Mdev),
		Expect: expect,
	}
	if c.Fwd >= 0 {
		n := c.Fwd
		l.Fwd = &n
	}
	if fwd >= 0 {
		v := round(fwd)
		l.LossFwd = &v
	}
	if rev >= 0 {
		v := round(rev)
		l.LossRev = &v
	}
	return l
}

// pathName renders a route as the dataset spells it: "hk>ty-a>ty-c", ids
// resolved to the names a person reads. An id this node has never been told about
// prints as itself rather than being dropped, because a path with a hole in it
// would silently look like a shorter one.
func (n *Node) pathName(path []uint8) string {
	parts := make([]string, 0, len(path))
	for _, id := range path {
		if name, ok := n.names[id]; ok {
			parts = append(parts, name)
			continue
		}
		parts = append(parts, fmt.Sprint(id))
	}
	return strings.Join(parts, ">")
}

// round keeps a line short. Three decimals is well past the resolution of a
// measurement made across an ocean.
func round(v float64) float64 { return math.Round(v*1000) / 1000 }

// dur prints a window the way it was configured rather than the way Go would:
// "10m", not "10m0s".
func dur(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}
