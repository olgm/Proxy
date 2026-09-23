package proxy

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/jsonl"
)

// live is the set of sessions this node is relaying right now. It belongs to the
// node rather than to a listener: a node may hold several ingresses, and "who is
// online here" is one answer across all of them.
//
// It is the only state about a session that outlives the goroutine relaying it,
// and it lasts exactly as long as that goroutine does. Nothing is remembered
// after a logout — the session log is what remembers.
//
// It is also how a shutdown reaches a session. The goroutine relaying one holds
// the only copy of what it cost until it returns, so a proxyd that exits with
// relays running loses every session it was carrying — which is what the feed
// had been showing as players who joined and never left.
type live struct {
	mu      sync.Mutex
	next    int64
	m       map[int64]open
	closing bool

	// pending counts sessions that have not finished being written down. It
	// outlives the register above by a hair: a session leaves m when its relay
	// ends, and leaves this count only once its record and its feed line are
	// out, which is the moment a shutdown may stop waiting for it.
	pending atomic.Int64

	// log is where a session goes when it ends, and history reads it back. Nil
	// on a node with no state directory, where history simply reports nothing.
	log  *jsonl.Writer
	path string
}

// open is one session in progress: what the control link reports about it, and
// how to end it. end closes both sides of the relay, not just the client —
// closing the client alone leaves the download direction blocked on a backend
// that has no reason to hang up.
type open struct {
	control.Live
	end func()
}

func newLive() *live { return &live{m: map[int64]open{}} }

// add records a session and returns the handle that removes it again. The caller
// must pair it with done, which is what says the session has been written down.
func (l *live) add(s Session, end func()) int64 {
	l.mu.Lock()
	l.next++
	id := l.next
	l.m[id] = open{Live: control.Live{Name: s.Name, UUID: s.UUID, IP: s.IP, Since: s.Start}, end: end}
	l.pending.Add(1)
	closing := l.closing
	l.mu.Unlock()
	// A login that got in behind the listener closing. Nothing is coming along
	// to end this one, so end it here: the relay returns at once and it is
	// written down like any other.
	if closing {
		end()
	}
	return id
}

func (l *live) remove(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, id)
}

// replace ends any session already open under the same identity, so an account
// holds one at a time, and reports how many it ended.
//
// A client that reconnects while its last attempt is still hanging open would
// otherwise hold two at once: the feed announced the join twice and reported the
// player as two online, and both were true of connections that really existed.
//
// The new connection wins. The old one is the one that stopped working — that is
// why there is a new one — and a player watching their client reconnect wants
// the reconnect to be the session that lives. It ends the ordinary way, so it is
// recorded and reported like any other.
//
// Identity is the uuid wherever there is one, and the name only where there is
// not. A route with no whitelist reads no Login Start and has neither; nothing
// there is claiming to be anybody, so nothing is replaced.
func (l *live) replace(uuid, name string) int {
	if uuid == "" && name == "" {
		return 0
	}
	l.mu.Lock()
	var ends []func()
	for _, o := range l.m {
		if uuid != "" && o.UUID != "" {
			if bare(o.UUID) != bare(uuid) {
				continue
			}
		} else if name == "" || !strings.EqualFold(o.Name, name) {
			continue
		}
		ends = append(ends, o.end)
	}
	l.mu.Unlock()
	for _, end := range ends {
		end()
	}
	return len(ends)
}

// done reports that a session has been written down. Deferred by the relay, so
// it happens whatever the relay does.
func (l *live) done() { l.pending.Add(-1) }

// endAll ends every session in progress, which is what makes each relay return
// and run its own logout: record, journal line, feed line. It does not wait —
// wait does that, and they are separate because those goroutines cannot finish
// while this one holds the lock.
func (l *live) endAll() {
	l.mu.Lock()
	l.closing = true
	ends := make([]func(), 0, len(l.m))
	for _, o := range l.m {
		ends = append(ends, o.end)
	}
	l.mu.Unlock()
	for _, end := range ends {
		end()
	}
}

// wait blocks until every session has been written down or d passes, and reports
// how many were still going when it gave up. Bounded on purpose: a session whose
// backend has stopped answering must not hold a restart open, because a restart
// that will not finish is killed, and being killed is the thing this avoids.
func (l *live) wait(d time.Duration) int64 {
	deadline := time.Now().Add(d)
	for {
		n := l.pending.Load()
		if n == 0 || time.Now().After(deadline) {
			return n
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// record writes a finished session down. It is the only thing that remembers one:
// the register above forgets it the moment the relay ends.
func (l *live) record(s Session) {
	if l.log == nil {
		return
	}
	l.log.Write(control.Past{
		Name: s.Name, UUID: s.UUID, IP: s.IP, Proto: s.Proto,
		Start: s.Start, End: s.End, Up: s.Up, Down: s.Down, Chain: s.Chain,
		RTT: s.RTT,
	})
}

// History returns the newest finished sessions belonging to any of uuids,
// newest first. A node with no log answers with nothing rather than refusing:
// "this node has no history" and "nobody by that name was here" are the same
// answer to the question actually being asked.
func (l *live) History(uuids []string, limit int) ([]control.Past, error) {
	if l.log == nil {
		return nil, nil
	}
	want := map[string]bool{}
	for _, u := range uuids {
		want[bare(u)] = true
	}
	keep := func(p *control.Past) bool { return len(want) == 0 || want[bare(p.UUID)] }

	// Tail gives them oldest first, because that is the order the file is in.
	got, err := jsonl.Tail(l.path, limit, keep)
	if err != nil {
		return nil, err
	}
	out := make([]control.Past, 0, len(got))
	for i := len(got) - 1; i >= 0; i-- {
		out = append(out, got[i])
	}
	return out, nil
}

// bare normalises a uuid so a dashed one and a bare one match. The file may hold
// either: what proxyd writes down is what the client claimed.
func bare(uuid string) string {
	return strings.ToLower(strings.ReplaceAll(uuid, "-", ""))
}

// Live answers the control link's sessions op, oldest first so a roster does not
// reshuffle every time it is redrawn.
func (l *live) Live() []control.Live {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]control.Live, 0, len(l.m))
	for _, o := range l.m {
		out = append(out, o.Live)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since.Equal(out[j].Since) {
			return out[i].Name < out[j].Name
		}
		return out[i].Since.Before(out[j].Since)
	})
	return out
}
