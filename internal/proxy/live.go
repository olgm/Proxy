package proxy

import (
	"sort"
	"sync"

	"github.com/olgm/proxy/internal/control"
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
