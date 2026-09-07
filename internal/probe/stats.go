package probe

import (
	"time"

	"github.com/olgm/proxy/internal/window"
)

// Report is one line of the dataset: one class over one window. The distribution
// is window.Closed's and the arithmetic behind it lives there; what this adds is
// which measurement produced it, which is the only part probed owns.
type Report struct {
	At     time.Time
	Window time.Duration
	Class  string
	Kind   string
	Dup    int

	// N is how many round trips the percentiles were computed from. It is in the
	// log line on purpose: p99 of sixty samples is the second-worst of sixty, and
	// a reader that cannot see n cannot know that.
	N    int
	Sent int
	Got  int
	// Fwd is how many probes reached the responder, or -1 when its counters could
	// not be read across this window. Sent - Fwd was lost on the way out and
	// Fwd - Got on the way back, which is the whole reason the answer carries
	// counters: a round trip alone cannot say which direction dropped.
	Fwd int

	Min, Max, Mean, P50, P90, P99, Mdev float64
}

// report labels a closed window. The fields are copied across rather than
// embedded so that this struct stays the flat shape the dataset and the feed
// were written against.
func report(c *window.Closed) *Report {
	return &Report{
		At:     c.At,
		Window: c.Window,
		N:      c.N,
		Sent:   c.Sent,
		Got:    c.Got,
		Fwd:    c.Fwd,
		Min:    c.Min,
		Max:    c.Max,
		Mean:   c.Mean,
		P50:    c.P50,
		P90:    c.P90,
		P99:    c.P99,
		Mdev:   c.Mdev,
	}
}

// Loss returns the three loss figures as percentages: out, back, and round trip.
// The first two are -1 when the responder's counters did not advance readably
// across the window, which is what a window with no answers at all looks like.
func (r *Report) Loss() (fwd, rev, rt float64) {
	return window.Closed{Sent: r.Sent, Got: r.Got, Fwd: r.Fwd}.Loss()
}
