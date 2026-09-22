package proxy

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/jsonl"
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
		Minecraft: &Minecraft{RewriteHost: "mc.example.com", RewritePort: 25565, Whitelist: list},
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

// The register holds a session for exactly as long as it is being relayed, and
// nothing after it. What remembers a finished session is the session log.
func TestLiveRegisterTracksASession(t *testing.T) {
	backend, got := fakeLoginBackend(t)
	ingress, s := feedIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"), "")

	if n := len(s.live.Live()); n != 0 {
		t.Fatalf("%d sessions before anyone logged in", n)
	}
	c := dialIngress(t, ingress, 764, mc.IntentLogin, loginStart("Notch", notchRaw[:]))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("login never reached the backend")
	}

	deadline := time.Now().Add(5 * time.Second)
	var live []control.Live
	for {
		if live = s.live.Live(); len(live) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d live sessions, want 1", len(live))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if live[0].Name != "Notch" || live[0].UUID != notchUUID {
		t.Errorf("live session came back wrong: %+v", live[0])
	}
	// The IP is here, where a manager's private reply can reach it. The feed's
	// own test asserts it never reaches a channel.
	if live[0].IP != "127.0.0.1" {
		t.Errorf("live session has no source: %+v", live[0])
	}

	c.Close()
	for s.online.Load() != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(s.live.Live()); n != 0 {
		t.Fatalf("%d sessions after the logout", n)
	}
}

// A finished session is written down, because the register above forgets it the
// moment the relay ends and /watch has to be able to look it up afterwards.
func TestFinishedSessionIsWrittenDown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	w, err := jsonl.NewWriter(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	backend, got := fakeLoginBackend(t)
	ingress, s := feedIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"), "")
	s.live.log, s.live.path = w, path

	c := dialIngress(t, ingress, 764, mc.IntentLogin, loginStart("Notch", notchRaw[:]))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("login never reached the backend")
	}
	c.Close()
	for s.online.Load() != 0 {
		time.Sleep(5 * time.Millisecond)
	}

	past, err := s.live.History(nil, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(past) != 1 {
		t.Fatalf("%d sessions written down, want 1", len(past))
	}
	p := past[0]
	if p.Name != "Notch" || p.UUID != notchUUID || p.IP != "127.0.0.1" {
		t.Errorf("session written down wrong: %+v", p)
	}
	if p.End.Before(p.Start) {
		t.Errorf("session ended before it started: %+v", p)
	}
	// A uuid nobody played under matches nothing, dashed or not.
	if got, _ := s.live.History([]string{"853c80ef-3c37-49fd-aa49-938b674adae6"}, 10); len(got) != 0 {
		t.Errorf("history matched the wrong player: %+v", got)
	}
	if got, _ := s.live.History([]string{strings.ReplaceAll(notchUUID, "-", "")}, 10); len(got) != 1 {
		t.Errorf("a bare uuid did not match the dashed one it was written as")
	}
}

// A client before 1.19 sends no UUID, so the session it opens would be written
// down under nobody — and `/watch` looks a session up by UUID, so nobody is who
// it would come back to. The whitelist matched the login to an identity in order
// to allow it at all; that identity is what the record carries.
func TestOldClientSessionIsRecordedUnderItsIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	w, err := jsonl.NewWriter(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	backend, got := fakeLoginBackend(t)
	ingress, s := feedIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"), "")
	s.live.log, s.live.path = w, path

	// 47 is 1.8.9: Login Start is the name and nothing else.
	c := dialIngress(t, ingress, 47, mc.IntentLogin, loginStart("Notch", nil))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("login never reached the backend")
	}
	c.Close()
	for s.online.Load() != 0 {
		time.Sleep(5 * time.Millisecond)
	}

	past, err := s.live.History([]string{notchUUID}, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(past) != 1 {
		t.Fatalf("%d sessions found by uuid, want 1: a 1.8 session is invisible to /watch", len(past))
	}
	if past[0].Proto != 47 || past[0].UUID != notchUUID {
		t.Errorf("session written down wrong: %+v", past[0])
	}
}
