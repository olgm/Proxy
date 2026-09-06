package proxy

import (
	"sort"
	"strings"
	"sync"

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
type live struct {
	mu   sync.Mutex
	next int64
	m    map[int64]control.Live

	// log is where a session goes when it ends, and history reads it back. Nil
	// on a node with no state directory, where history simply reports nothing.
	log  *jsonl.Writer
	path string
}

func newLive() *live { return &live{m: map[int64]control.Live{}} }

// add records a session and returns the handle that removes it again.
func (l *live) add(s Session) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	l.m[l.next] = control.Live{Name: s.Name, UUID: s.UUID, IP: s.IP, Since: s.Start}
	return l.next
}

func (l *live) remove(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, id)
}

// record writes a finished session down. It is the only thing that remembers one:
// the register above forgets it the moment the relay ends.
func (l *live) record(s Session) {
	if l.log == nil {
		return
	}
	l.log.Write(control.Past{
		Name: s.Name, UUID: s.UUID, IP: s.IP, Proto: s.Proto,
		Start: s.Start, End: s.End, Up: s.Up, Down: s.Down, Wire: s.Wire,
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
	for _, s := range l.m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since.Equal(out[j].Since) {
			return out[i].Name < out[j].Name
		}
		return out[i].Since.Before(out[j].Since)
	})
	return out
}
