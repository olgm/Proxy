package mojang

import (
	"sync"
	"time"
)

// A lookup is only ever triggered by a login that already failed the local check,
// so every one of them is attacker-reachable. These limiters decide how much of
// that a stranger can turn into outbound requests.
const (
	globalN      = 50
	globalWindow = 5 * time.Minute

	// evictAfter is how long a key must sit untouched before its bucket is dropped.
	// By then it would have decayed to the first rung anyway, so nothing is lost.
	evictAfter = 4 * time.Hour
	maxKeys    = 4096
)

// tier is one rung of the escalation ladder: at most n lookups per window.
type tier struct {
	n      int
	window time.Duration
}

// ladder tightens on a key that keeps missing. Spending a rung denies that request
// and moves the key down, with the longer window starting already spent — hitting a
// limit means waiting it out, not being handed a fresh budget. The last rung repeats,
// re-arming every time it is spent. A window that elapses with no attempt at all
// moves the key back up one rung: the ladder answers sustained pressure, and should
// not pin someone who renamed, retried twice and went away.
var ladder = []tier{
	{3, 5 * time.Minute},
	{2, time.Hour},
	{1, time.Hour},
}

type bucket struct {
	rung      int
	spent     int  // lookups allowed in this window
	attempts  int  // lookups asked for in this window, allowed or not
	escalated bool // already moved down a rung this window
	reset     time.Time
}

// Gate rate-limits Mojang lookups per name, per source IP, and overall.
type Gate struct {
	mu     sync.Mutex
	names  map[string]*bucket
	ips    map[string]*bucket
	global bucket
	now    func() time.Time // swapped in tests
}

func NewGate() *Gate {
	return &Gate{
		names: map[string]*bucket{},
		ips:   map[string]*bucket{},
		now:   time.Now,
	}
}

// Allow reports whether one lookup for name from ip may proceed, and spends budget
// from all three limiters when it does. Nothing is spent unless every limiter
// agrees, so a request refused by one does not burn the others.
func (g *Gate) Allow(name, ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()

	// The global cap has no ladder and no decay: once spent, every lookup waits for
	// the window to roll. It is the ceiling on what we can ever send Mojang in five
	// minutes, however many names or addresses are trying.
	if now.After(g.global.reset) {
		g.global = bucket{reset: now.Add(globalWindow)}
	}
	if g.global.spent >= globalN {
		return false
	}

	nb := g.roll(g.names, name, now)
	ib := g.roll(g.ips, ip, now)
	nb.attempts++
	ib.attempts++

	nOK := nb.spent < ladder[nb.rung].n
	iOK := ib.spent < ladder[ib.rung].n
	if !nOK || !iOK {
		// Escalate only the limiter that actually ran out.
		if !nOK {
			escalate(nb, now)
		}
		if !iOK {
			escalate(ib, now)
		}
		return false
	}

	nb.spent++
	ib.spent++
	g.global.spent++
	return true
}

// roll returns the key's bucket with its window brought up to date.
func (g *Gate) roll(m map[string]*bucket, key string, now time.Time) *bucket {
	b, ok := m[key]
	if !ok {
		if len(m) >= maxKeys {
			evict(m, now)
		}
		b = &bucket{reset: now.Add(ladder[0].window)}
		m[key] = b
		return b
	}
	if now.After(b.reset) {
		// Untouched for a whole window: ease off one rung. Still being hammered:
		// stay where we are and hand back the current rung's budget.
		if b.attempts == 0 && b.rung > 0 {
			b.rung--
		}
		b.spent, b.attempts, b.escalated = 0, 0, false
		b.reset = now.Add(ladder[b.rung].window)
	}
	return b
}

// escalate moves a spent key down a rung, once per window. The bottom rung repeats,
// re-arming its window, so sustained pressure stays throttled rather than expiring.
func escalate(b *bucket, now time.Time) {
	if b.escalated {
		return
	}
	if b.rung < len(ladder)-1 {
		b.rung++
	}
	b.escalated = true
	b.spent = ladder[b.rung].n // no budget until the new window rolls
	// Attempts start over with the window: what matters for decay is whether the
	// key keeps pushing from here, not what it did to earn the rung.
	b.attempts = 0
	b.reset = now.Add(ladder[b.rung].window)
}

func evict(m map[string]*bucket, now time.Time) {
	for k, b := range m {
		if now.After(b.reset.Add(evictAfter)) {
			delete(m, k)
		}
	}
}
