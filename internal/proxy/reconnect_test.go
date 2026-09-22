package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/mc"
)

// One account, one session. A client that reconnects while its last attempt is
// still hanging open must not hold two at once, or the feed announces the join
// twice and the roster reports one player as two.
func TestReconnectReplacesTheSessionItLeftOpen(t *testing.T) {
	feed := newFeedCapture(t)
	ingress, s := feedIngress(t, holdingBackend(t), whitelistFile(t, "Notch:"+notchUUID+"\n"), feed.url)

	first := dialIngress(t, ingress, 764, mc.IntentLogin, loginStart("Notch", notchRaw[:]))
	waitPending(t, s.live, 1)
	began := s.live.Live()[0].Since

	// The same player again, without the first one having gone anywhere.
	dialIngress(t, ingress, 764, mc.IntentLogin, loginStart("Notch", notchRaw[:]))

	// Settle on one session, which is the new one, with nothing else still being
	// written down: the replaced session ended the ordinary way rather than being
	// dropped, and the new one is what lived. The old one is the one that stopped
	// working, which is why there was a new one at all.
	deadline := time.Now().Add(5 * time.Second)
	for {
		live := s.live.Live()
		if len(live) == 1 && live[0].Since.After(began) && s.live.pending.Load() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions open, %d unfinished; want one new one and nothing pending",
				len(live), s.live.pending.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}

	first.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := first.Read(make([]byte, 16)); err == nil {
		t.Error("the replaced connection is still open")
	}

	s.feed.Close()
	all := feed.all()
	if strings.Count(all, "joined") != 2 || strings.Count(all, "left") != 1 {
		t.Errorf("want two joins and the leave they caused:\n%s", all)
	}
}

// waitPending blocks until the node has n sessions it has not finished writing
// down. It is the register's own signal that a session is completely over, which
// online is not: online falls before the record and the feed line are out.
func waitPending(t *testing.T, l *live, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for l.pending.Load() != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d unfinished sessions, want %d", l.pending.Load(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
