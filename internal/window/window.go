// Package window aggregates timed probes into fixed windows and closes each one
// into a distribution.
//
// It was probed's stats.go, moved here when triald needed to set a candidate
// node's numbers beside probed's numbers for the node it would replace. That
// comparison is only sound if both sides computed their percentiles the same
// way, and two copies of a percentile drift apart the moment one of them is
// touched. So the arithmetic lives once, and both services read it from here.
//
// Two rules govern every number that leaves this package. A sample is filed
// under the time it was sent. A window is not closed until one probe timeout
// after it ends. Both exist because getting them wrong reports loss on a link
// that is dropping nothing.
package window

import (
	"math"
	"sort"
	"time"
)

// maxSamples bounds one bucket. At 1 Hz a ten-minute window holds 600 samples;
// this only matters if somebody sets a rate high enough that a window would
// otherwise grow without limit. Past it the percentiles are computed from what
// was kept, and N says how many that was.
const maxSamples = 1 << 20

// Sample is one resolved probe. It is filed under the time it was *sent*, never
// the time it came back: a probe answered 300 ms after a window closed still
// says something about the window it was sent in, and filing it by arrival would
// move every slow sample one window to the right — which is exactly how a
// latency spike gets attributed to the calm minute after it.
type Sample struct {
	At   time.Time
	RTT  time.Duration
	Lost bool
	// Recv is the responder's count of probes it had accepted when it answered,
	// or zero if this probe was never answered.
	Recv uint64
}

// bucket is one measurement over one window.
type bucket struct {
	sent, got int
	recvHi    uint64
	rtts      []float64
}

// Series aggregates one measurement over one window length. Buckets are keyed by
// the window index the sample's send time falls in, so a late answer lands in the
// right one without any need to keep windows open in order.
type Series struct {
	d       time.Duration
	open    map[int64]*bucket
	carry   uint64 // responder's count at the end of the last flushed window
	carried bool
}

func NewSeries(d time.Duration) *Series {
	return &Series{d: d, open: map[int64]*bucket{}}
}

// Length is the window this series closes on. Callers need it to label the line
// they write, and it is the only reason it is readable at all.
func (s *Series) Length() time.Duration { return s.d }

func (s *Series) index(t time.Time) int64 { return t.UnixNano() / int64(s.d) }

func (s *Series) Add(smp Sample) {
	i := s.index(smp.At)
	b := s.open[i]
	if b == nil {
		b = &bucket{}
		s.open[i] = b
	}
	b.sent++
	if smp.Lost {
		return
	}
	b.got++
	if smp.Recv > b.recvHi {
		b.recvHi = smp.Recv
	}
	if len(b.rtts) < maxSamples {
		b.rtts = append(b.rtts, float64(smp.RTT)/float64(time.Millisecond))
	}
}

// Due returns the windows that have closed and whose probes have all had time to
// either answer or time out, oldest first. Holding the flush back by the probe
// timeout is what keeps a probe still in flight at the boundary from being counted
// as lost in the window it was sent in.
func (s *Series) Due(now time.Time, timeout time.Duration) []int64 {
	var out []int64
	for i := range s.open {
		end := time.Unix(0, (i+1)*int64(s.d))
		if now.After(end.Add(timeout)) {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

// Flush closes one window and produces its distribution. It must be called in
// ascending index order, because the count of probes that arrived is the
// difference of two of the responder's counters and the earlier one comes from
// the window before.
func (s *Series) Flush(i int64) *Closed {
	b := s.open[i]
	delete(s.open, i)
	if b == nil || b.sent == 0 {
		return nil
	}

	c := &Closed{
		At:     time.Unix(0, (i+1)*int64(s.d)).UTC(),
		Window: s.d,
		N:      len(b.rtts),
		Sent:   b.sent,
		Got:    b.got,
		Fwd:    -1,
	}

	// The responder counts probes it accepted, so the difference across a window
	// is how many arrived. A window with no answer at all leaves the counter
	// where it was rather than reading as zero arrivals.
	hi := b.recvHi
	if hi == 0 {
		hi = s.carry
	}
	if s.carried && hi >= s.carry {
		if fwd := int(hi - s.carry); fwd <= b.sent {
			c.Fwd = fwd
		}
	}
	s.carry, s.carried = hi, true

	c.summarise(b.rtts)
	return c
}

// Closed is one window that has ended, with the distribution of what it held.
// What measurement produced it is the caller's to say: this package knows only
// the numbers.
type Closed struct {
	At     time.Time
	Window time.Duration

	// N is how many round trips the percentiles were computed from. Carry it into
	// whatever line gets written: p99 of sixty samples is the second-worst of
	// sixty, and a reader who cannot see N cannot know that.
	N    int
	Sent int
	Got  int
	// Fwd is how many probes reached the responder, or -1 when its counters could
	// not be read across this window. Sent - Fwd was lost on the way out and
	// Fwd - Got on the way back, which is the whole reason an answer carries
	// counters: a round trip alone cannot say which direction dropped.
	Fwd int

	Min, Max, Mean, P50, P90, P99, Mdev float64
}

func (c *Closed) summarise(rtts []float64) {
	if len(rtts) == 0 {
		return
	}
	sorted := append([]float64(nil), rtts...)
	sort.Float64s(sorted)
	var sum, sumsq float64
	for _, v := range sorted {
		sum += v
		sumsq += v * v
	}
	n := float64(len(sorted))
	c.Mean = sum / n
	// mdev as ping(8) reports it: population standard deviation. proxyd's own
	// link line reports a *smoothed* mean deviation instead, so the two are
	// different numbers over the same leg and should not be compared directly.
	c.Mdev = math.Sqrt(math.Max(0, sumsq/n-c.Mean*c.Mean))
	c.Min, c.Max = sorted[0], sorted[len(sorted)-1]
	c.P50, c.P90, c.P99 = pct(sorted, 50), pct(sorted, 90), pct(sorted, 99)
}

// pct indexes the sorted samples the way tools/tcpping does, so a number from one
// can be put beside a number from the other.
func pct(sorted []float64, p int) float64 {
	return sorted[p*(len(sorted)-1)/100]
}

// Loss returns the three loss figures as percentages: out, back, and round trip.
// The first two are -1 when the responder's counters did not advance readably
// across the window, which is what a window with no answers at all looks like.
func (c Closed) Loss() (fwd, rev, rt float64) {
	rt = 100 * float64(c.Sent-c.Got) / float64(c.Sent)
	if c.Fwd < 0 {
		return -1, -1, rt
	}
	fwd = 100 * float64(c.Sent-c.Fwd) / float64(c.Sent)
	rev = -1
	if c.Fwd > 0 {
		rev = 100 * float64(c.Fwd-c.Got) / float64(c.Fwd)
	}
	return fwd, rev, rt
}
