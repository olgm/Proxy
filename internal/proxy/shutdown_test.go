package proxy

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/mc"
)

// A node that stops must not take its sessions with it.
//
// proxyd is stopped by a systemctl restart on every deploy, and the goroutine
// relaying a session holds the only copy of what that session cost until it
// returns. Being killed mid-relay therefore lost the record, the journal line
// and the feed line together — leaving the feed full of players who joined and
// never left, and /watch with no history for a session that plainly happened.
func TestShutdownWritesDownTheSessionsItEnds(t *testing.T) {
	useMojang(t, nil)
	feed := newFeedCapture(t)
	t.Setenv(EnvSessionsWebhook, feed.url)

	cfg := &Config{
		Name:       "hk",
		SessionLog: filepath.Join(t.TempDir(), "sessions.jsonl"),
		Listeners: []Listener{{
			Bind:     "127.0.0.1:0",
			Upstream: holdingBackend(t),
			Minecraft: &Minecraft{
				RewriteHost: "mc.example.com", RewritePort: 25565,
				Whitelist: whitelistFile(t, "Notch:"+notchUUID+"\n"),
			},
		}},
	}
	n, err := start(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dialIngress(t, n.lns[0].Addr().String(), 764, mc.IntentLogin, loginStart("Notch", notchRaw[:]))
	deadline := time.Now().Add(5 * time.Second)
	for len(n.live.Live()) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the session never opened")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Stopped with the session still relaying, which is what every deploy does.
	n.close()
	n.wg.Wait()

	past, err := n.live.History([]string{notchUUID}, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(past) != 1 {
		t.Fatalf("%d sessions written down over a shutdown, want 1", len(past))
	}
	if past[0].Name != "Notch" || past[0].End.Before(past[0].Start) {
		t.Errorf("session written down wrong: %+v", past[0])
	}
	if all := feed.all(); !strings.Contains(all, "left") {
		t.Errorf("the feed never said the session ended: %q", all)
	}
}

// Run has to come back when it is told to, not only when a listener dies.
// Nothing asks proxyd to stop but a signal, and a signal it cannot act on is the
// same as not handling one.
func TestRunReturnsWhenStopped(t *testing.T) {
	useMojang(t, nil)
	backend, _ := fakeLoginBackend(t)
	cfg := &Config{Name: "hk", Listeners: []Listener{{Bind: "127.0.0.1:0", Upstream: backend}}}

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- Run(cfg, stop) }()
	close(stop)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when it was stopped")
	}
}
