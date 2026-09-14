package tunnel

import (
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrUnrepairable means a chunk fell out of every buffer that could have
	// replaced it. An ordered stream cannot continue past that, so the session
	// ends and the player reconnects. Plain TCP has the same failure; it only
	// hides it for longer.
	ErrUnrepairable = errors.New("tunnel: stream is missing data no hop can still supply")
	ErrReset        = errors.New("tunnel: reset by peer")
	ErrClosed       = errors.New("tunnel: stream closed")
)

const (
	// maxProbes bounds the originator's blind retransmits before it gives up on
	// the path entirely. Backed off, it works out near the repair deadline.
	maxProbes = 8
	// linger keeps a closed stream able to answer a late NACK, and keeps a
	// retransmitted chunk from being mistaken for a new stream and dialled again.
	linger = 60 * time.Second
	// maxHoles bounds how far ahead of itself a peer may claim to be.
	maxHoles = 1 << 16
)

type chunk struct {
	seq    uint64
	flags  byte
	data   []byte
	sentAt time.Time
	rtx    bool
}

// hole is a sequence number we have not seen but know exists, because something
// above it arrived.
type hole struct {
	first time.Time // when the gap appeared; the repair deadline runs from here
	last  time.Time // when we last asked for it
}

// dir is one direction of one stream as this node sees it.
//
// send is where the data leaves, back is where it arrived from. Whether those are
// empty is the node's entire role:
//
//	back empty -> this node originates the direction (the entry going out, the
//	              exit coming back), assigns sequence numbers and duplicates
//	send empty -> the direction terminates here and must be put back in order
//	neither    -> a relay: forward the first copy of each number on arrival, drop
//	              the rest, keep it for the next hop to ask for
type dir struct {
	s    *Stream
	send []*Link
	back []*Link

	mu   sync.Mutex
	cond *sync.Cond

	// Outbound state: chunks we have put on send and may be asked for again.
	next    uint64
	buf     map[uint64]*chunk
	bufSize int
	acked   uint64 // every sequence below this is delivered at the far end
	probes  int
	moved   time.Time // last time acked advanced, or the first send
	srtt    time.Duration
	mdev    time.Duration
	// lastSent and lastHead drive the horizon advert: it goes out once the
	// direction has been quiet for HeadQuiet, then again every leg RTO.
	lastSent time.Time
	lastHead time.Time

	// Inbound state.
	top     uint64 // one past the highest sequence seen
	want    map[uint64]*hole
	pending map[uint64]*chunk // terminators only: arrived, not yet in order
	deliver uint64            // terminators only: next sequence the reader gets
	head    []byte
	eof     bool
	lastAck time.Time
	sentAck uint64

	err error
}

func newDir(s *Stream, send, back []*Link) *dir {
	d := &dir{s: s, send: send, back: back,
		buf: map[uint64]*chunk{}, want: map[uint64]*hole{}}
	if len(send) == 0 {
		d.pending = map[uint64]*chunk{}
	}
	d.cond = sync.NewCond(&d.mu)
	return d
}

func (d *dir) originates() bool { return len(d.back) == 0 }
func (d *dir) terminates() bool { return len(d.send) == 0 }

// write splits the stream into chunks and sends each one. It blocks while the far
// end is behind by a window's worth of bytes, which is what pushes back on the
// player's socket when the exit cannot drain into the backend fast enough.
func (d *dir) write(p []byte) (int, error) {
	opt := &d.s.n.opt
	total := 0
	for len(p) > 0 {
		take := min(len(p), opt.maxChunk)
		d.mu.Lock()
		for d.bufSize >= opt.Window && d.err == nil {
			d.cond.Wait()
		}
		if d.err != nil {
			err := d.err
			d.mu.Unlock()
			return total, err
		}
		c := &chunk{seq: d.next, data: slices.Clone(p[:take]), sentAt: time.Now()}
		d.next++
		d.buf[c.seq] = c
		d.bufSize += take
		if d.moved.IsZero() {
			d.moved = c.sentAt
		}
		d.mu.Unlock()

		d.transmit(c, false)
		p = p[take:]
		total += take
	}
	return total, nil
}

// finish sends the end of the direction as an ordinary numbered chunk, so it
// arrives after everything before it and never ahead of a retransmission.
func (d *dir) finish() error {
	d.mu.Lock()
	if d.err != nil {
		err := d.err
		d.mu.Unlock()
		return err
	}
	c := &chunk{seq: d.next, flags: flagFin, sentAt: time.Now()}
	d.next++
	d.buf[c.seq] = c
	if d.moved.IsZero() {
		d.moved = c.sentAt
	}
	d.mu.Unlock()
	d.transmit(c, false)
	return nil
}

// transmit puts a chunk on every outgoing link, as many times as that link is
// configured to carry it. The count is the link's, not the chunk's: a chunk that
// arrived twice from a lossy leg leaves once onto a clean one.
func (d *dir) transmit(c *chunk, rtx bool) {
	plain := encode(d.s.id, c, rtx)
	for _, l := range d.send {
		d.s.sent.Add(uint64(l.send(plain, l.dup, rtx)))
	}
	d.mu.Lock()
	d.lastSent = time.Now()
	d.mu.Unlock()
}

// horizon is one past the highest sequence this node has put on its send links:
// what it numbered where it originates, what it has seen where it forwards.
func (d *dir) horizon() uint64 {
	if d.originates() {
		return d.next
	}
	return d.top
}

func encode(stream uint64, c *chunk, rtx bool) []byte {
	flags := c.flags
	if rtx {
		flags |= flagRtx
	}
	return appendData(nil, stream, c.seq, flags, c.data)
}

func (d *dir) recv(p packet) {
	now := time.Now()
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		return
	}
	if !d.advance(p.seq+1, now) {
		return
	}
	delete(d.want, p.seq)

	if d.terminates() {
		_, dup := d.pending[p.seq]
		if p.seq < d.deliver || dup {
			d.mu.Unlock() // another copy of something we already have
			return
		}
		d.pending[p.seq] = &chunk{seq: p.seq, flags: p.flags, data: slices.Clone(p.payload)}
		d.cond.Broadcast()
		d.mu.Unlock()
		return
	}

	// A relay. The first copy of a number is kept, so the next hop can ask for it,
	// and sent on with this leg's own copy count. Every later copy is dropped
	// here: the buffer is the record of what has been seen, and anything below
	// the watermark has already been delivered past us. A re-send is not a copy:
	// the originator probes its highest chunk blind when nothing is being
	// acknowledged, and if the hop after us is the one that lost it, dropping the
	// probe here would leave the exit's horizon short of the tail for ever.
	if p.seq < d.acked {
		d.mu.Unlock()
		return
	}
	if c, held := d.buf[p.seq]; held {
		d.mu.Unlock()
		if p.flags&flagRtx != 0 {
			d.transmit(c, true)
		}
		return
	}
	c := &chunk{seq: p.seq, flags: p.flags &^ flagRtx, data: slices.Clone(p.payload)}
	d.buf[p.seq] = c
	d.bufSize += len(c.data)
	d.mu.Unlock()
	d.transmit(c, false)
}

// advance moves the horizon to top. Anything between the highest we had seen and
// there is a hole. This is the whole detector: 100 and 102 arrive, so 101 is
// missing and gets asked for. Called with the lock held; reports false, with the
// lock released, if the stream had to be aborted.
func (d *dir) advance(top uint64, now time.Time) bool {
	if top <= d.top {
		return true
	}
	// The window stops any correct sender getting this far ahead, so a jump this
	// large is a broken peer, and walking to it would hang the socket loop rather
	// than lose a stream.
	if top-d.top > maxHoles {
		d.mu.Unlock()
		d.s.abort(ErrUnrepairable)
		return false
	}
	for s := d.top; s < top; s++ {
		d.want[s] = &hole{first: now}
	}
	d.top = top
	return true
}

// onHead learns how far the previous hop got. A tail it lost is now a set of
// ordinary holes, asked for on the next tick.
func (d *dir) onHead(top uint64) {
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		return
	}
	if d.advance(top, time.Now()) {
		d.mu.Unlock()
	}
}

func (d *dir) read(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		if len(d.head) > 0 {
			n := copy(p, d.head)
			d.head = d.head[n:]
			return n, nil
		}
		if c, ok := d.pending[d.deliver]; ok {
			delete(d.pending, d.deliver)
			d.deliver++
			if c.flags&flagFin != 0 {
				d.eof = true
			}
			d.head = c.data
			continue
		}
		if d.eof {
			return 0, io.EOF
		}
		if d.err != nil {
			return 0, d.err
		}
		d.cond.Wait()
	}
}

// onAck frees everything the far end has already read, and reports whether the
// watermark moved — a repeat carries no news and is not worth passing on.
func (d *dir) onAck(through uint64) bool {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if through <= d.acked {
		return false
	}
	// A chunk that was only ever sent once gives an honest end-to-end round trip.
	if c, ok := d.buf[through-1]; ok && !c.rtx {
		d.sampleRTT(now.Sub(c.sentAt))
	}
	for s := d.acked; s < through; s++ {
		if c, ok := d.buf[s]; ok {
			d.bufSize -= len(c.data)
			delete(d.buf, s)
		}
	}
	d.acked = through
	d.probes = 0
	d.moved = now
	d.cond.Broadcast()
	return true
}

func (d *dir) onNack(l *Link, seqs []uint64) {
	now := time.Now()
	var resend []*chunk
	d.mu.Lock()
	for _, s := range seqs {
		if s < d.acked {
			continue
		}
		if c, ok := d.buf[s]; ok {
			c.rtx = true
			resend = append(resend, c)
			continue
		}
		if d.originates() {
			continue // out of our buffer; nothing can bring it back
		}
		// We never had it either. Want it ourselves, so the repair walks one leg
		// further back instead of the next hop asking us forever.
		if _, ok := d.want[s]; !ok {
			d.want[s] = &hole{first: now}
		}
		if s >= d.top {
			d.top = s + 1
		}
	}
	d.mu.Unlock()
	for _, c := range resend {
		d.s.sent.Add(uint64(l.send(encode(d.s.id, c, true), l.dup, true)))
	}
}

// tick drives everything that is a timer rather than a packet: asking again for a
// hole, reporting the watermark, and probing a path that has gone quiet.
func (d *dir) tick(now time.Time) {
	var (
		ask   []uint64
		ack   uint64
		head  uint64
		probe *chunk
		dead  bool
	)
	t := &d.s.n.opt.Timers
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		return
	}
	interval := rtoOf(d.back)
	for s, h := range d.want {
		if now.Sub(h.first) > d.s.n.opt.Repair {
			if d.terminates() {
				dead = true
				break
			}
			delete(d.want, s) // a relay just stops asking; the far end still can
			continue
		}
		if !h.last.IsZero() && now.Sub(h.last) < interval {
			continue
		}
		if len(ask) < maxNackSeqs {
			h.last = now
			ask = append(ask, s)
		}
	}
	if d.terminates() && d.deliver > 0 && now.Sub(d.lastAck) >= t.AckEvery &&
		(d.deliver > d.sentAck || now.Sub(d.lastAck) >= t.AckRepeat) {
		ack, d.sentAck, d.lastAck = d.deliver, d.deliver, now
	}
	// Quiet with chunks the far end has not acknowledged: say how far we got, so
	// a tail lost on this leg is asked for on this leg, and again every leg RTO
	// while it stays unacknowledged, in case the advert itself was lost.
	if !d.terminates() && d.acked < d.horizon() && now.Sub(d.lastSent) >= t.HeadQuiet {
		wait := t.HeadQuiet
		if d.lastHead.After(d.lastSent) {
			wait = rtoOf(d.send)
		}
		if now.Sub(d.lastHead) >= wait {
			head, d.lastHead = d.horizon(), now
		}
	}
	if d.originates() && d.acked < d.next && !d.moved.IsZero() {
		if now.Sub(d.moved) >= d.probeWait() {
			if d.probes >= maxProbes {
				dead = true
			} else {
				d.probes++
				d.moved = now
				// Re-send the *highest* chunk, not the lowest outstanding one.
				// Gap detection is blind past the last sequence that arrived, so a
				// receiver cannot ask for a tail it has never seen the far side of.
				// One copy of the last chunk moves its horizon to the end of what
				// we sent, and every hole under it becomes an ordinary NACK.
				if c, ok := d.buf[d.next-1]; ok {
					c.rtx = true
					probe = c
				}
			}
		}
	}
	d.mu.Unlock()

	if dead {
		d.s.abort(ErrUnrepairable)
		return
	}
	if len(ask) > 0 {
		slices.Sort(ask)
		plain := appendNack(nil, d.s.id, ask)
		for _, l := range d.back {
			d.s.sent.Add(uint64(l.send(plain, 1, false)))
		}
	}
	if ack > 0 {
		plain := appendAck(nil, d.s.id, ack)
		for _, l := range d.back {
			d.s.sent.Add(uint64(l.send(plain, 1, false)))
		}
	}
	if head > 0 {
		plain := appendHead(nil, d.s.id, head)
		for _, l := range d.send {
			d.s.sent.Add(uint64(l.send(plain, 1, false)))
		}
	}
	if probe != nil {
		d.transmit(probe, true)
	}
}

func (d *dir) abort(err error) {
	d.mu.Lock()
	if d.err == nil {
		d.err = err
	}
	d.cond.Broadcast()
	d.mu.Unlock()
}

// sampleRTT and rto track the whole path, not one leg: the sample is the time
// from sending a chunk to the far end reporting it delivered.
func (d *dir) sampleRTT(r time.Duration) {
	if d.srtt == 0 {
		d.srtt, d.mdev = r, r/2
		return
	}
	diff := d.srtt - r
	if diff < 0 {
		diff = -diff
	}
	d.mdev = (3*d.mdev + diff) / 4
	d.srtt = (7*d.srtt + r) / 8
}

func (d *dir) probeWait() time.Duration {
	t := &d.s.n.opt.Timers
	base := 500 * time.Millisecond
	if d.srtt > 0 {
		base = d.srtt + 4*d.mdev
	}
	return clamp(base<<min(d.probes, 3), t.ProbeMin, t.ProbeMax)
}

func rtoOf(links []*Link) time.Duration {
	out := time.Duration(1<<63 - 1)
	for _, l := range links {
		out = min(out, l.rto())
	}
	return out
}

// Stream is one client connection carried through the tunnel. At the entry and
// the exit it is a byte pipe that can half-close, so it substitutes for the TCP
// connection the plain-TCP path would have dialled. At a relay nobody reads or
// writes it; it only holds the buffers the two directions pass through.
type Stream struct {
	n  *Node
	id uint64
	// down carries data toward the exit, up carries it back toward the entry.
	down, up *dir
	// rx and tx are the terminal view of those two. Nil at a relay.
	rx, tx *dir

	// sent and recv are every byte this node has put on a socket for this stream
	// and taken off one: each chunk once per copy the leg is set to carry, plus
	// every re-send, plus the acks and nacks that keep it repaired, all sealed.
	// Payload says what the session carried; these say what carrying it cost,
	// which is the only way to see what a duplicate count is actually buying —
	// and, added up across the chain, what a session costs in billed traffic.
	//
	// Link keepalives are not here: a ping belongs to the leg, not to a session,
	// and it is sent whether anyone is playing or not.
	sent, recv atomic.Uint64

	mu       sync.Mutex
	lastSeen time.Time
	expires  time.Time // once closed, state lingers to answer late NACKs
}

// Traffic is what one node spent on one stream: what it put on its legs and what
// it took off them, as the legs carried it rather than as the session read it.
type Traffic struct{ Sent, Recv uint64 }

// Total is what both halves cost together.
func (t Traffic) Total() uint64 { return t.Sent + t.Recv }

// ID is the number every node on the chain knows this stream by. Each one's log
// line for a session carries it, which is what lets the entry's account of an
// ending be read beside the exit's.
func (s *Stream) ID() uint64 { return s.id }

// Traffic reports what this stream has cost this node on the tunnel's legs,
// duplicates and re-sends included. The ingress logs it beside the payload at
// logout, where it is the measured leg the rest of the chain is reckoned from.
func (s *Stream) Traffic() Traffic {
	return Traffic{Sent: s.sent.Load(), Recv: s.recv.Load()}
}

func (s *Stream) Read(p []byte) (int, error) {
	if s.rx == nil {
		return 0, ErrClosed
	}
	return s.rx.read(p)
}

func (s *Stream) Write(p []byte) (int, error) {
	if s.tx == nil {
		return 0, ErrClosed
	}
	return s.tx.write(p)
}

// CloseWrite ends our half. The other direction keeps flowing, which is what lets
// a client stop talking without cutting off what the server is still sending.
func (s *Stream) CloseWrite() error {
	if s.tx == nil {
		return ErrClosed
	}
	return s.tx.finish()
}

// Close releases the stream. A stream that ended cleanly is not reset: both ends
// already saw the FIN in order, and a reset racing a retransmission would truncate
// it. State lingers either way so a late NACK still finds an answer.
func (s *Stream) Close() error {
	reset := false
	if s.rx != nil {
		s.rx.mu.Lock()
		reset = !s.rx.eof
		s.rx.mu.Unlock()
	}
	s.n.close(s, reset)
	return nil
}

func (s *Stream) abort(err error) {
	// A session that ends early otherwise leaves no trace at all: the reader above
	// sees a closed connection and cannot tell a finished stream from a broken one.
	if err != ErrClosed {
		log.Printf("%s: stream %016x: %v (%s)", s.n.opt.Name, s.id, err, s.progress())
	}
	s.down.abort(err)
	s.up.abort(err)
	s.n.close(s, err != ErrReset)
}

func (s *Stream) progress() string {
	return fmt.Sprintf("%d chunks toward the exit, %d back", s.down.progress(), s.up.progress())
}

// progress is how many chunks this direction has accounted for, which means
// something different at each role and is the right number in all three: sent
// where the direction originates, handed over where it terminates, seen in
// between.
func (d *dir) progress() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.originates():
		return d.next
	case d.terminates():
		return d.deliver
	default:
		return d.top
	}
}

func (s *Stream) touch(now time.Time) {
	s.mu.Lock()
	s.lastSeen = now
	s.mu.Unlock()
}

func (s *Stream) seen() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeen
}
