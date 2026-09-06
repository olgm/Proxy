package proxy

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/mc"
)

// feedCapture stands in for Discord and keeps every message body posted to it.
type feedCapture struct {
	mu   sync.Mutex
	msgs []string
	url  string
}

func newFeedCapture(t *testing.T) *feedCapture {
	t.Helper()
	c := &feedCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p struct {
			Content string `json:"content"`
		}
		json.Unmarshal(b, &p)
		c.mu.Lock()
		c.msgs = append(c.msgs, p.Content)
		c.mu.Unlock()
		w.Write([]byte(`{"id":"1"}`))
	}))
	t.Cleanup(srv.Close)
	c.url = srv.URL
	return c
}

func (c *feedCapture) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.msgs, "\n")
}

// feedIngress is an ingress with the session feed pointed at a capture.
func feedIngress(t *testing.T, upstream, list, url string) (string, *server) {
	t.Helper()
	if _, stubbed := newMojang().(stubMojang); !stubbed {
		useMojang(t, nil)
	}
	l := Listener{
		Bind:      "127.0.0.1:0",
		Upstream:  upstream,
		Minecraft: &Minecraft{RewriteHost: "mc.hypixel.net", RewritePort: 25565, Whitelist: list},
	}
	s, err := newServer(l)
	if err != nil {
		t.Fatal(err)
	}
	s.node, s.feed = "hk", newSessionFeed(url)
	ln, err := net.Listen("tcp", l.Bind)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go s.accept(ln)
	return ln.Addr().String(), s
}

// drain waits for the session to end and flushes whatever the feed has queued.
func drain(t *testing.T, s *server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.online.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("session never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.feed.Close()
}

func TestSessionFeedPostsLoginAndLogout(t *testing.T) {
	cap := newFeedCapture(t)
	backend, got := fakeLoginBackend(t)
	ingress, s := feedIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"), cap.url)

	c := dialIngress(t, ingress, 764, mc.IntentLogin, loginStart("Notch", notchRaw[:]))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("login never reached the backend")
	}
	// The backend has hung up; the player hanging up is what ends the session.
	c.Close()
	drain(t, s)

	all := cap.all()
	for _, want := range []string{"**hk**", "`Notch`", "joined", "left", notchUUID, "online 1", "online 0"} {
		if !strings.Contains(all, want) {
			t.Errorf("feed is missing %q:\n%s", want, all)
		}
	}
}

// The client IP is in the journal, where an operator with the node already has
// it, and in /watch, which answers one manager privately. A channel is neither,
// and this is the assertion that keeps it that way.
func TestSessionFeedNeverCarriesTheClientIP(t *testing.T) {
	cap := newFeedCapture(t)
	backend, got := fakeLoginBackend(t)
	ingress, s := feedIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"), cap.url)

	c := dialIngress(t, ingress, 764, mc.IntentLogin, loginStart("Notch", notchRaw[:]))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("login never reached the backend")
	}
	// The backend has hung up; the player hanging up is what ends the session.
	c.Close()
	drain(t, s)

	if all := cap.all(); strings.Contains(all, "127.0.0.1") {
		t.Fatalf("the feed carries the client IP:\n%s", all)
	}
}

// An IGN may contain an underscore, and Discord reads that as markup: `_x_` in
// bold comes out italic. Inline code is the one span nothing is parsed inside.
func TestSessionFeedRendersNamesSafely(t *testing.T) {
	if got := code("_x_"); got != "`_x_`" {
		t.Errorf("code(%q) = %q, want it in an inline code span", "_x_", got)
	}
	// A backtick would close that span; no Mojang name has one, but a legacy
	// name is not something to take on trust.
	if got := code("a`b"); strings.Count(got, "`") != 2 {
		t.Errorf("code(%q) = %q, which escapes its own span", "a`b", got)
	}
	// A route with no whitelist never reads Login Start, so it has no name and
	// no uuid — and must not post an empty pair of backticks for either.
	if got := code(""); got != "a session" {
		t.Errorf("code(%q) = %q", "", got)
	}
	if got := uuidPart(""); got != "" {
		t.Errorf("uuidPart(%q) = %q, want nothing at all", "", got)
	}
}

// A feed nobody configured has to cost the relay path nothing, including the
// nil check at every call site.
func TestNoFeedIsNoFeed(t *testing.T) {
	f := newSessionFeed("")
	if f != nil {
		t.Fatal("an unset URL produced a feed")
	}
	f.login(Session{Name: "Notch"})
	f.logout(Session{Name: "Notch"})
	f.Close()
}
