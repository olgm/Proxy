package probe

import (
	"math"
	"sort"
	"time"
)

// maxSamples bounds one bucket. At the default rate a ten-minute window holds 600
// samples; this only matters if somebody sets Hz high enough that a window would
// otherwise grow without limit. Past it the percentiles are computed from what was
// kept, and n in the log line says how many that was.
const maxSamples = 1 << 20

// sample is one resolved probe. It is filed under the time it was *sent*, never
// the time it came back: a probe answered 300 ms after a window closed still says
// something about the window it was sent in, and filing it by arrival would move
// every slow sample one window to the right — which is exactly how a latency spike
// gets attributed to the calm minute after it.
type sample struct {
	at   time.Time
	rtt  time.Duration
	lost bool
	// recv is the responder's count of distinct probes of this class it had
	// accepted when it answered, or zero if this probe was never answered.
	recv uint64
}

// bucket is one class over one window.
type bucket struct {
	sent, got int
	recvHi    uint64
	rtts      []float64
}

// series aggregates one class over one window length. Buckets are keyed by the
// window index the sample's send time falls in, so a late answer lands in the
// right one without any need to keep windows open in order.
type series struct {
	d       time.Duration
	open    map[int64]*bucket
	carry   uint64 // responder's count at the end of the last flushed window
	carried bool
}

func newSeries(d time.Duration) *series {
	return &series{d: d, open: map[int64]*bucket{}}
}

func (s *series) index(t time.Time) int64 { return t.UnixNano() / int64(s.d) }

func (s *series) add(smp sample) {
	i := s.index(smp.at)
	b := s.open[i]
	if b == nil {
		b = &bucket{}
		s.open[i] = b
	}
	b.sent++
	if smp.lost {
		return
	}
	b.got++
	if smp.recv > b.recvHi {
		b.recvHi = smp.recv
	}
	if len(b.rtts) < maxSamples {
		b.rtts = append(b.rtts, float64(smp.rtt)/float64(time.Millisecond))
	}
}

// due returns the windows that have closed and whose probes have all had time to
// either answer or time out, oldest first. Holding the flush back by the probe
// timeout is what keeps a probe still in flight at the boundary from being counted
// as lost in the window it was sent in.
func (s *series) due(now time.Time, timeout time.Duration) []int64 {
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

// flush closes one window and produces its line. It must be called in ascending
// index order, because the count of probes that arrived is the difference of two
// of the responder's counters and the earlier one comes from the window before.
func (s *series) flush(i int64) *Report {
	b := s.open[i]
	delete(s.open, i)
	if b == nil || b.sent == 0 {
		return nil
	}

	r := &Report{
		At:     time.Unix(0, (i+1)*int64(s.d)).UTC(),
		Window: s.d,
		N:      len(b.rtts),
		Sent:   b.sent,
		Got:    b.got,
		Fwd:    -1,
	}

	// The responder counts distinct probes it accepted, so the difference across a
	// window is how many arrived — after duplication, which is the number that
	// matters. A window with no answer at all leaves the counter where it was
	// rather than reading as zero arrivals.
	hi := b.recvHi
	if hi == 0 {
		hi = s.carry
	}
	if s.carried && hi >= s.carry {
		if fwd := int(hi - s.carry); fwd <= b.sent {
			r.Fwd = fwd
		}
	}
	s.carry, s.carried = hi, true

	r.summarise(b.rtts)
	return r
}

// Report is one line of the dataset: one class over one window.
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

func (r *Report) summarise(rtts []float64) {
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
	r.Mean = sum / n
	// mdev as ping(8) reports it: population standard deviation. proxyd's own
	// link line reports a *smoothed* mean deviation instead, so the two are
	// different numbers over the same leg and should not be compared directly.
	r.Mdev = math.Sqrt(math.Max(0, sumsq/n-r.Mean*r.Mean))
	r.Min, r.Max = sorted[0], sorted[len(sorted)-1]
	r.P50, r.P90, r.P99 = pct(sorted, 50), pct(sorted, 90), pct(sorted, 99)
}

// pct indexes the sorted samples the way tools/tcpping does, so a number from one
// can be put beside a number from the other.
func pct(sorted []float64, p int) float64 {
	return sorted[p*(len(sorted)-1)/100]
}

// Loss returns the three loss figures as percentages: out, back, and round trip.
// The first two are -1 when the responder's counters did not advance readably
// across the window, which is what a window with no answers at all looks like.
func (r *Report) Loss() (fwd, rev, rt float64) {
	rt = 100 * float64(r.Sent-r.Got) / float64(r.Sent)
	if r.Fwd < 0 {
		return -1, -1, rt
	}
	fwd = 100 * float64(r.Sent-r.Fwd) / float64(r.Sent)
	rev = -1
	if r.Fwd > 0 {
		rev = 100 * float64(r.Fwd-r.Got) / float64(r.Fwd)
	}
	return fwd, rev, rt
}
