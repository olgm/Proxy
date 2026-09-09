package control

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/mojang"
	"github.com/olgm/proxy/internal/sealed"
	"github.com/olgm/proxy/internal/tunnel"
	"github.com/olgm/proxy/internal/whitelist"
)

const (
	notchUUID = "069a79f4-44e9-4726-a5be-fca90e38aaf5"
	notchBare = "069a79f444e94726a5befca90e38aaf5"
	alexUUID  = "853c80ef-3c37-49fd-aa49-938b674adae6"
)

type fakeMojang struct {
	owners map[string]string // lower-cased name -> bare uuid
	names  map[string]string // bare uuid -> name
	down   bool
	calls  int
}

func (f *fakeMojang) LookupName(name string) (string, string, error) {
	f.calls++
	if f.down {
		return "", "", errors.New("mojang: 502 Bad Gateway")
	}
	u, ok := f.owners[strings.ToLower(name)]
	if !ok {
		return "", "", mojang.ErrNoSuchPlayer
	}
	return u, f.names[u], nil
}

func (f *fakeMojang) LookupUUID(uuid string) (string, error) {
	f.calls++
	if f.down {
		return "", errors.New("mojang: 502 Bad Gateway")
	}
	n, ok := f.names[strings.ReplaceAll(uuid, "-", "")]
	if !ok {
		return "", mojang.ErrNoSuchPlayer
	}
	return n, nil
}

func notchOnly() *fakeMojang {
	return &fakeMojang{
		owners: map[string]string{"notch": notchBare},
		names:  map[string]string{notchBare: "Notch"},
	}
}

// testLive is what every served node in these tests says is online. A node with
// no ingress passes nil instead, which is a different answer and is tested for.
var testLive = stubSessions{{Name: "Notch", UUID: notchUUID, IP: "203.0.113.9",
	Since: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}}

type stubSessions []Live

func (s stubSessions) Live() []Live { return s }

// testPast is what every served node in these tests has written down.
var testPast = []Past{{Name: "Notch", UUID: notchUUID, IP: "203.0.113.9",
	Start: time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC),
	End:   time.Date(2026, 9, 5, 11, 42, 0, 0, time.UTC), Up: 4200, Down: 51700, Chain: 111600}}

func (s stubSessions) History(uuids []string, limit int) ([]Past, error) {
	if len(uuids) == 0 {
		return testPast, nil
	}
	for _, u := range uuids {
		if u == notchUUID {
			return testPast, nil
		}
	}
	return nil, nil
}

// serve starts a server over a fresh list and returns a client holding its key,
// the list's path, and the raw address for tests that speak the wire themselves.
func serve(t *testing.T, body string, r Resolver) (*Client, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "whitelist.txt")
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	l, err := whitelist.Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := tunnel.NewKey()
	s, err := NewServer(l, key, nil, r, testLive)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go s.Serve(ln)
	return &Client{Addr: ln.Addr().String(), Key: key}, p
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestListAddRemove(t *testing.T) {
	c, p := serve(t, "# friends\nAlex:"+alexUUID+"\n", notchOnly())

	es, err := c.List()
	if err != nil || len(es) != 1 || es[0].Name != "Alex" {
		t.Fatalf("List = %+v, %v", es, err)
	}

	// By name: resolved, spelt Mojang's way, uuid dashed, tag kept.
	rep, err := c.Add("notch", "", "discord:42")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.Entry == nil || rep.Entry.Name != "Notch" || rep.Entry.UUID != notchUUID || rep.Entry.Tag != "discord:42" {
		t.Fatalf("Add = %+v", rep)
	}
	if got, want := read(t, p), "# friends\nAlex:"+alexUUID+"\nNotch:"+notchUUID+" # discord:42\n"; got != want {
		t.Fatalf("file is %q, want %q", got, want)
	}

	// The same uuid again, however spelt, is refused with the line it has.
	rep, err = c.Add("", notchBare, "discord:7")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || !rep.Listed || rep.Entry == nil || rep.Entry.Tag != "discord:42" {
		t.Fatalf("second Add = %+v", rep)
	}

	rep, err = c.Remove(notchBare)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.Entry == nil || rep.Entry.Name != "Notch" {
		t.Fatalf("Remove = %+v", rep)
	}
	if got, want := read(t, p), "# friends\nAlex:"+alexUUID+"\n"; got != want {
		t.Fatalf("file is %q, want %q", got, want)
	}
	rep, err = c.Remove(notchUUID)
	if err != nil || rep.OK || rep.Error != "not listed" {
		t.Fatalf("Remove again = %+v, %v", rep, err)
	}
}

func TestAddByUUIDResolvesTheName(t *testing.T) {
	c, _ := serve(t, "", notchOnly())
	rep, err := c.Add("", notchUUID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.Entry.Name != "Notch" {
		t.Fatalf("Add = %+v", rep)
	}
}

func TestAddWithBothNeedsNoResolver(t *testing.T) {
	c, _ := serve(t, "", nil)
	rep, err := c.Add("Notch", notchUUID, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("Add = %+v", rep)
	}
	for _, req := range []Request{
		{Op: "add", Name: "Alex"},
		{Op: "add", UUID: alexUUID},
		{Op: "add"},
	} {
		rep, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if rep.OK {
			t.Errorf("%+v accepted without a resolver", req)
		}
	}
}

// A member who mistyped is told so; a member who caught Mojang down is told that.
func TestUnknownAndUnreachableAreDifferentAnswers(t *testing.T) {
	m := notchOnly()
	c, _ := serve(t, "", m)
	rep, err := c.Add("Herobrine", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || !strings.Contains(rep.Error, "no such player") {
		t.Fatalf("unknown name: %+v", rep)
	}

	m.down = true
	rep, err = c.Add("notch", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || !strings.Contains(rep.Error, "502") {
		t.Fatalf("mojang down: %+v", rep)
	}

	// Junk is refused before it costs a lookup.
	before := m.calls
	for _, name := range []string{"has space", "seventeen_chars_x", "semi;colon", ""} {
		rep, err := c.Add(name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if rep.OK {
			t.Errorf("%q accepted", name)
		}
	}
	if m.calls != before {
		t.Errorf("%d lookups for junk names", m.calls-before)
	}
}

func TestWrongKeyGetsNoReply(t *testing.T) {
	c, p := serve(t, "", nil)
	wrong := &Client{Addr: c.Addr, Key: tunnel.NewKey()}
	if _, err := wrong.Add("Notch", notchUUID, ""); err == nil {
		t.Fatal("a stranger's request got a reply")
	}
	if read(t, p) != "" {
		t.Fatal("a stranger's request changed the list")
	}
	// The right key still works afterwards.
	if _, err := c.List(); err != nil {
		t.Fatal(err)
	}
}

// exchange speaks the wire by hand so a test can keep a frame and send it again.
func exchange(t *testing.T, addr string, frame []byte) ([]byte, []byte, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	chal := make([]byte, sealed.ChallengeLen)
	if _, err := io.ReadFull(conn, chal); err != nil {
		t.Fatal(err)
	}
	if err := sealed.WriteFrame(conn, frame); err != nil {
		t.Fatal(err)
	}
	rep, err := sealed.ReadFrame(conn)
	return chal, rep, err
}

func TestReplayIsRefused(t *testing.T) {
	c, p := serve(t, "", nil)
	s, _ := sealed.NewSealer(c.Key)
	req, _ := json.Marshal(Request{Op: "add", Name: "Notch", UUID: notchUUID})

	// A frame has to be sealed against the challenge of the connection it is sent
	// on, which means building it after the challenge arrives.
	conn, err := net.Dial("tcp", c.Addr)
	if err != nil {
		t.Fatal(err)
	}
	chal := make([]byte, sealed.ChallengeLen)
	if _, err := io.ReadFull(conn, chal); err != nil {
		t.Fatal(err)
	}
	frame := s.Seal(req, sealed.AAD(chal, sealed.DirRequest))
	if err := sealed.WriteFrame(conn, frame); err != nil {
		t.Fatal(err)
	}
	if _, err := sealed.ReadFrame(conn); err != nil {
		t.Fatalf("first send: %v", err)
	}
	conn.Close()
	if !strings.Contains(read(t, p), "Notch") {
		t.Fatal("first send did not add")
	}

	// The same bytes on a new connection meet a new challenge and open for nobody.
	if err := os.WriteFile(p, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, rep, err := exchange(t, c.Addr, frame); err == nil {
		t.Fatalf("replay got a reply: %x", rep)
	}
	if read(t, p) != "" {
		t.Fatal("replay changed the list")
	}
}

func TestReplyCannotBeReplayedAsRequest(t *testing.T) {
	c, _ := serve(t, "", nil)
	s, _ := sealed.NewSealer(c.Key)
	chal := make([]byte, sealed.ChallengeLen)
	// A frame sealed as a reply, even for the right challenge, is not a request.
	body, _ := json.Marshal(Request{Op: "list"})
	if _, err := s.Open(s.Seal(body, sealed.AAD(chal, sealed.DirReply)), sealed.AAD(chal, sealed.DirRequest)); err == nil {
		t.Fatal("direction is not part of what is authenticated")
	}
}

func TestAllowed(t *testing.T) {
	s, err := NewServer(nil, tunnel.NewKey(), []netip.Prefix{netip.MustParsePrefix("198.51.100.10/32")}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"::ffff:127.0.0.1", true},
		{"198.51.100.10", true},
		{"198.51.100.11", false},
		{"10.0.0.1", false},
	} {
		a := &net.TCPAddr{IP: net.ParseIP(tc.ip), Port: 1}
		if got := s.allowed(a); got != tc.want {
			t.Errorf("allowed(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
	if s.allowed(&net.UDPAddr{IP: net.ParseIP("127.0.0.1")}) {
		t.Error("a udp address is not a completed handshake")
	}
}

func TestFind(t *testing.T) {
	es := []whitelist.Entry{
		{Name: "Notch", UUID: notchUUID, Tag: "discord:42"},
		{Name: "Alex", UUID: strings.ReplaceAll(alexUUID, "-", "")},
	}
	for _, q := range []string{"notch", "NOTCH", notchUUID, notchBare, strings.ToUpper(notchBare)} {
		if e, ok := Find(es, q); !ok || e.Name != "Notch" {
			t.Errorf("Find(%q) = %+v, %v", q, e, ok)
		}
	}
	if e, ok := Find(es, alexUUID); !ok || e.Name != "Alex" {
		t.Errorf("Find(dashed) against a bare entry = %+v, %v", e, ok)
	}
	if _, ok := Find(es, "Herobrine"); ok {
		t.Error("Find of a stranger succeeded")
	}
}

func TestIsUUIDAndDashed(t *testing.T) {
	if !IsUUID(notchUUID) || !IsUUID(notchBare) || !IsUUID(strings.ToUpper(notchBare)) {
		t.Error("real uuids not recognised")
	}
	if IsUUID("Notch") || IsUUID(notchBare[:31]) || IsUUID("g"+notchBare[1:]) {
		t.Error("junk recognised as a uuid")
	}
	if got := dashed(strings.ToUpper(notchBare)); got != notchUUID {
		t.Errorf("dashed = %q", got)
	}
	if got := dashed("short"); got != "short" {
		t.Errorf("dashed left junk as %q", got)
	}
}

func TestSessionsReportsWhoIsOnline(t *testing.T) {
	c, _ := serve(t, "Notch:"+notchUUID+"\n", notchOnly())
	live, err := c.Sessions()
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("got %d live sessions, want 1", len(live))
	}
	if live[0].Name != "Notch" || live[0].UUID != notchUUID || live[0].IP != "203.0.113.9" {
		t.Fatalf("live session came back wrong: %+v", live[0])
	}
	if live[0].Since.IsZero() {
		t.Error("a live session with no start time cannot be shown as a duration")
	}
}

// "Nobody is online here" and "I cannot tell you" are different answers, and a
// roster that merges them would quietly lose a node.
func TestSessionsRefusedByANodeThatRelaysNone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "whitelist.txt")
	if err := os.WriteFile(p, []byte("Notch:"+notchUUID+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	l, err := whitelist.Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := tunnel.NewKey()
	s, err := NewServer(l, key, nil, notchOnly(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go s.Serve(ln)

	if _, err := (&Client{Addr: ln.Addr().String(), Key: key}).Sessions(); err == nil {
		t.Fatal("a node with no ingress answered the sessions op")
	}
}

func TestHistoryReturnsFinishedSessions(t *testing.T) {
	c, _ := serve(t, "Notch:"+notchUUID+"\n", notchOnly())

	past, err := c.History(nil, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(past) != 1 || past[0].Name != "Notch" {
		t.Fatalf("history = %+v", past)
	}
	if past[0].Up != 4200 || past[0].Chain != 111600 {
		t.Errorf("a finished session should carry what it cost: %+v", past[0])
	}

	// Filtered to somebody who was never here.
	got, err := c.History([]string{alexUUID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("history matched the wrong player: %+v", got)
	}
}

// A caller that asks for the world gets a page, not the world: the reply has to
// fit in one sealed frame and a manager is reading it.
func TestHistoryIsCapped(t *testing.T) {
	var asked int
	s := &Server{live: capturingSessions{&asked}}
	s.apply(Request{Op: "history", Limit: 100000})
	if asked > maxHistory {
		t.Fatalf("asked the node for %d sessions, past the %d cap", asked, maxHistory)
	}
	// And a caller that asks for nothing still gets something back.
	asked = 0
	s.apply(Request{Op: "history"})
	if asked <= 0 {
		t.Fatalf("a limit of zero asked for %d", asked)
	}
}

type capturingSessions struct{ limit *int }

func (c capturingSessions) Live() []Live { return nil }
func (c capturingSessions) History(_ []string, limit int) ([]Past, error) {
	*c.limit = limit
	return nil, nil
}
