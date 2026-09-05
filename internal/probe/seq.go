package probe

// seqWindow tells a copy of a probe from a probe not seen before. It is the same
// sliding-bitmap shape as the tunnel's replay window and deliberately not the same
// thing: that one exists to stop a captured datagram being re-injected, this one to
// drop the second copy of a deliberately duplicated probe. Sharing the mechanism
// and not the purpose is why it is written out again here rather than exported.
//
// Index 0 is the highest sequence seen; index i is that many below it. Anything
// older than the window is treated as a copy, which is the safe way to be wrong:
// counting a probe twice would understate loss.
type seqWindow struct {
	set  bool
	high uint64
	bits [64]uint64
}

const seqWindowSize = 64 * 64

// accept reports whether this sequence number is new, and records it.
func (w *seqWindow) accept(seq uint64) bool {
	if !w.set {
		*w = seqWindow{set: true, high: seq}
		w.mark(0)
		return true
	}
	switch {
	case seq > w.high:
		w.shift(seq - w.high)
		w.high = seq
		w.mark(0)
		return true
	case w.high-seq >= seqWindowSize:
		return false
	default:
		i := int(w.high - seq)
		if w.test(i) {
			return false
		}
		w.mark(i)
		return true
	}
}

func (w *seqWindow) mark(i int) { w.bits[i/64] |= 1 << (i % 64) }

func (w *seqWindow) test(i int) bool { return w.bits[i/64]&(1<<(i%64)) != 0 }

// shift moves every recorded sequence n places further into the past, which is
// what advancing the top of the window does to everything under it.
func (w *seqWindow) shift(n uint64) {
	if n >= seqWindowSize {
		w.bits = [64]uint64{}
		return
	}
	words, off := int(n/64), uint(n%64)
	for i := len(w.bits) - 1; i >= 0; i-- {
		var v uint64
		if i-words >= 0 {
			v = w.bits[i-words] << off
			if off > 0 && i-words-1 >= 0 {
				v |= w.bits[i-words-1] >> (64 - off)
			}
		}
		w.bits[i] = v
	}
}
