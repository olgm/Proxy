package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/probe"
	"github.com/olgm/proxy/internal/webhook"
)

// A refused connection is the kernel saying the host is here and nothing is on
// that port. Silence is the host itself being gone. Reporting those the same way
// would lose the distinction the status feed exists for.
func TestClassifyTellsARefusalFromSilence(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want reach
	}{
		{"nothing wrong", nil, reachOK},
		{"refused", &net.OpError{Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, reachRefused},
		{"host unreachable", &net.OpError{Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}, reachUnreachable},
		{"timed out", &net.OpError{Err: &timeoutErr{}}, reachUnreachable},
		{"talked, then failed", errors.New("reply does not open"), reachBroken},
	} {
		if got := classify(tc.err); got != tc.want {
			t.Errorf("%s: classify = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type timeoutErr struct{}

func (t *timeoutErr) Error() string   { return "i/o timeout" }
func (t *timeoutErr) Timeout() bool   { return true }
func (t *timeoutErr) Temporary() bool { return true }

// fakeFeed is the status channel: every call in the order it was made, which is
// what most of these tests are actually about.
type fakeFeed struct {
	acts []string
	sent map[string]webhook.Message
	n    int
}

func newFakeFeed() *fakeFeed { return &fakeFeed{sent: map[string]webhook.Message{}} }

func (f *fakeFeed) Send(m webhook.Message) (string, error) {
	f.n++
	id := fmt.Sprintf("m%d", f.n)
	f.sent[id] = m
	f.acts = append(f.acts, "send "+id+" "+describe(m))
	return id, nil
}

func (f *fakeFeed) Update(id string, m webhook.Message) error {
	f.sent[id] = m
	f.acts = append(f.acts, "update "+id+" "+describe(m))
	return nil
}

func (f *fakeFeed) Delete(id string) error {
	delete(f.sent, id)
	f.acts = append(f.acts, "delete "+id)
	return nil
}

func describe(m webhook.Message) string {
	if m.Image != nil {
		return "<card>"
	}
	return m.Content
}

// kinds is the sequence of verbs, with card uploads collapsed to "card": the
// order of delete, line and card is the whole design.
func (f *fakeFeed) kinds() []string {
	out := make([]string, 0, len(f.acts))
	for _, a := range f.acts {
		verb, rest, _ := strings.Cut(a, " ")
		_, body, _ := strings.Cut(rest, " ")
		switch {
		case verb == "delete":
			out = append(out, "delete")
		case body == "<card>":
			out = append(out, verb+" card")
		default:
			out = append(out, verb+" line")
		}
	}
	return out
}

func testWatcher(t *testing.T, ping string) (*watcher, *fakeFeed) {
	t.Helper()
	f := newFakeFeed()
	return &watcher{
		feed: f, ping: ping,
		st:       openStore(filepath.Join(t.TempDir(), "feeds.json")),
		state:    map[string]string{},
		strikes:  map[string]int{},
		seen:     map[string]bool{},
		badSince: map[string]time.Time{},
		classes:  map[string]int{},
	}, f
}

// up and down are one node's snapshot, healthy and unreachable.
func up(node string, players int) snap {
	live := make([]control.Live, players)
	return snap{
		node: node, live: live, hasProbe: true,
		health: probe.Health{
			Node: node, Classes: 2,
			Legs: []probe.Leg{{Class: node + ">ch", Kind: probe.KindLeg, Window: "10m", P50: 42.5, N: 600}},
		},
	}
}

func down(node string) snap {
	err := &net.OpError{Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}
	return snap{node: node, err: err, hasProbe: true, healthErr: err}
}

// One dropped packet is not an outage. A change has to hold for several dials
// before anyone is told about it.
func TestStatusWaitsForAChangeToHold(t *testing.T) {
	w, f := testWatcher(t, "")
	now := time.Now()
	w.apply([]snap{up("hk", 0)}, now) // baseline: healthy, and silent about it

	for i := 0; i < statusStrikes-1; i++ {
		w.apply([]snap{down("hk")}, now)
	}
	if lines(f) != 0 {
		t.Fatalf("announced before the change held: %v", f.acts)
	}
	w.apply([]snap{down("hk")}, now)
	if lines(f) != 1 {
		t.Fatalf("did not announce a held change: %v", f.acts)
	}

	// And a flap that goes back before the strikes run out says nothing more.
	w.apply([]snap{up("hk", 0)}, now)
	w.apply([]snap{down("hk")}, now)
	if lines(f) != 1 {
		t.Fatalf("a flap was announced: %v", f.acts)
	}
}

func lines(f *fakeFeed) int {
	n := 0
	for _, k := range f.kinds() {
		if k == "send line" {
			n++
		}
	}
	return n
}

// A node that is already down when the bot starts is news, so the first
// observation is announced when it is a fault — and only then.
func TestStatusAnnouncesAFaultItStartedWith(t *testing.T) {
	w, f := testWatcher(t, "")
	w.apply([]snap{down("hk")}, time.Now())
	if lines(f) != 1 {
		t.Fatalf("a node already down at startup was not reported: %v", f.acts)
	}

	w2, quiet := testWatcher(t, "")
	w2.apply([]snap{up("hk", 0)}, time.Now())
	if lines(quiet) != 0 {
		t.Fatalf("a healthy node announced itself at startup: %v", quiet.acts)
	}
}

// The order is the design: the card comes down, the news goes in, the card goes
// back at the foot. Anything else leaves the card above the line explaining it.
func TestStatusPutsTheCardBackAtTheFoot(t *testing.T) {
	w, f := testWatcher(t, "")
	now := time.Now()
	w.apply([]snap{up("hk", 0)}, now) // posts the first card

	for i := 0; i < statusStrikes; i++ {
		w.apply([]snap{down("hk")}, now)
	}
	want := []string{"send card", "delete", "send line", "send card"}
	if got := f.kinds(); !equal(got, want) {
		t.Fatalf("sequence was %v, want %v", got, want)
	}
}

// With nothing to report the card is edited where it is, and nothing else is
// posted at all.
func TestQuietTickEditsTheCardInPlace(t *testing.T) {
	w, f := testWatcher(t, "")
	now := time.Now()
	w.apply([]snap{up("hk", 0)}, now)
	w.apply([]snap{up("hk", 0)}, now.Add(boardEvery))

	want := []string{"send card", "update card"}
	if got := f.kinds(); !equal(got, want) {
		t.Fatalf("sequence was %v, want %v", got, want)
	}
}

// A tick sooner than the redraw interval does not re-upload the same picture.
func TestCardIsNotRedrawnFasterThanItChanges(t *testing.T) {
	w, f := testWatcher(t, "")
	now := time.Now()
	w.apply([]snap{up("hk", 0)}, now)
	w.apply([]snap{up("hk", 0)}, now.Add(boardEvery/2))

	if got := f.kinds(); !equal(got, []string{"send card"}) {
		t.Fatalf("sequence was %v, want one card", got)
	}
}

// A closed incident is a log entry, and a log entry that still looks like an
// alert makes the next real alert easier to miss. Both halves go small and grey.
func TestRecoveryQuietensBothHalves(t *testing.T) {
	w, f := testWatcher(t, "")
	now := time.Now()
	w.apply([]snap{down("hk")}, now)
	offline := lastLine(f)

	for i := 0; i < statusStrikes; i++ {
		w.apply([]snap{up("hk", 0)}, now)
	}
	online := lastLine(f)

	for _, id := range []string{offline, online} {
		got := f.sent[id].Content
		if !strings.HasPrefix(got, "> -# ") {
			t.Errorf("%s was left standing: %q", id, got)
		}
	}
	if !strings.Contains(f.sent[offline].Content, "is offline") {
		t.Errorf("the quietened fault no longer says what it was: %q", f.sent[offline].Content)
	}
	if !strings.Contains(f.sent[online].Content, "is online") {
		t.Errorf("the quietened recovery says %q", f.sent[online].Content)
	}
	// And the incident is closed, so a restart would not adopt it.
	if is, ok := w.st.get().Issues["hk"]; ok {
		t.Errorf("the incident is still open: %+v", is)
	}
}

// An open fault stays open in the state file, which is what stops a restart
// announcing it a second time.
func TestOpenFaultSurvivesARestart(t *testing.T) {
	w, f := testWatcher(t, "")
	w.apply([]snap{down("hk")}, time.Now())
	if is := w.st.get().Issues["hk"]; is.Message == "" || !strings.Contains(is.Cond, "no answer") {
		t.Fatalf("the fault was not recorded: %+v", is)
	}

	// A second watcher over the same file adopts it and says nothing.
	w2 := &watcher{
		feed: f, st: w.st,
		state: map[string]string{}, strikes: map[string]int{}, seen: map[string]bool{},
		badSince: map[string]time.Time{}, classes: map[string]int{},
	}
	for node, is := range w.st.get().Issues {
		w2.seen[node], w2.state[node] = true, is.Cond
	}
	before := lines(f)
	w2.apply([]snap{down("hk")}, time.Now())
	if lines(f) != before {
		t.Fatalf("a fault already announced was announced again: %v", f.acts)
	}
}

// The role is opt-in, and it is the only thing a status line may ever notify:
// everything else, including anything in a fault that looks like a mention,
// stays suppressed.
func TestPingIsOptInAndScoped(t *testing.T) {
	w, f := testWatcher(t, "")
	w.apply([]snap{down("hk")}, time.Now())
	m := f.sent[lastLine(f)]
	if m.Ping != nil || strings.Contains(m.Content, "<@&") {
		t.Fatalf("an unset role still pinged: %+v", m)
	}

	w2, f2 := testWatcher(t, "42")
	w2.apply([]snap{down("hk")}, time.Now())
	m2 := f2.sent[lastLine(f2)]
	if !strings.HasPrefix(m2.Content, "<@&42> ") {
		t.Errorf("the line does not mention the role: %q", m2.Content)
	}
	if len(m2.Ping) != 1 || m2.Ping[0] != "42" {
		t.Errorf("allowed mentions = %v, want just the role", m2.Ping)
	}
}

// An unreachable node must report once, not once per service on it: probed is
// not asked about when the node itself did not answer.
func TestUnreachableNodeReportsOnce(t *testing.T) {
	w, f := testWatcher(t, "")
	w.apply([]snap{down("hk")}, time.Now())
	if lines(f) != 1 {
		t.Fatalf("want one line for one fault, got %v", f.acts)
	}
	if c := f.sent[lastLine(f)].Content; strings.Contains(c, "probed") {
		t.Errorf("probed was reported for an unreachable node: %q", c)
	}
}

func lastLine(f *fakeFeed) string {
	last := ""
	for _, a := range f.acts {
		verb, rest, _ := strings.Cut(a, " ")
		id, body, _ := strings.Cut(rest, " ")
		if verb == "send" && body != "<card>" {
			last = id
		}
	}
	return last
}

// The row shows the longest path a node originates: its own route to the exit,
// which is what "how is hk doing" means. An exit originates nothing and has no
// leg at all.
func TestPickLegTakesTheWholeRoute(t *testing.T) {
	legs := []probe.Leg{
		{Class: "hk>ty", Kind: probe.KindLeg, Window: "10m", P50: 44, N: 600},
		{Class: "hk>ch", Kind: probe.KindChain, Window: "10m", P50: 220.4, N: 600},
	}
	name, ms := pickLeg(legs)
	if name != "hk→ch" || ms != 220.4 {
		t.Errorf("leg = %q %v, want hk→ch 220.4", name, ms)
	}
	if name, ms := pickLeg(nil); name != "" || ms != -1 {
		t.Errorf("an exit reported %q %v", name, ms)
	}
}

// A probed that stopped measuring keeps reporting its last window forever. The
// card would rather say nothing than say something that stopped being true.
func TestStaleLatencyIsDropped(t *testing.T) {
	fresh := probe.Leg{Class: "au>ch", Window: "10m", P50: 176.5, N: 600, AgeSecs: 120}
	old := probe.Leg{Class: "au>ch", Window: "10m", P50: 176.5, N: 600, AgeSecs: 3600}
	if _, ms := pickLeg([]probe.Leg{fresh}); ms != 176.5 {
		t.Errorf("a fresh window was dropped: %v", ms)
	}
	if _, ms := pickLeg([]probe.Leg{old}); ms != -1 {
		t.Errorf("an hour-old ten-minute window was still shown: %v", ms)
	}
	// A class that has measured nothing yet is listed with no figure, not a zero.
	if _, ms := pickLeg([]probe.Leg{{Class: "au>ch", P50: -1, N: -1, AgeSecs: -1}}); ms != -1 {
		t.Errorf("a class with nothing measured reported %v", ms)
	}
}

// A window in which every probe was lost still closes: it has sends behind it and
// no round trips, so its percentile is zero. A leg that carried nothing must not
// read as the fastest one on the card.
func TestATotallyLostWindowIsNotZeroMilliseconds(t *testing.T) {
	lost := probe.Leg{Class: "au>ch", Window: "10m", P50: 0, N: 0, Loss: 100, AgeSecs: 60}
	if _, ms := pickLeg([]probe.Leg{lost}); ms != -1 {
		t.Errorf("a leg at 100%% loss reported %v ms", ms)
	}
}

// The footer counts what is configured against what is being measured, so a node
// whose probed is down still holds its place in the denominator.
func TestCardCountsClassesThatStoppedReporting(t *testing.T) {
	w, _ := testWatcher(t, "")
	now := time.Now()
	w.apply([]snap{up("au", 0), up("hk", 1)}, now)

	sick := up("hk", 1)
	sick.healthErr = errors.New("refused")
	d := w.card([]snap{up("au", 0), sick}, now)
	if d.classesTotal != 4 || d.classesUp != 2 {
		t.Errorf("classes = %d/%d, want 2/4", d.classesUp, d.classesTotal)
	}
	if d.nodesUp != 1 || d.nodesTotal != 2 {
		t.Errorf("nodes = %d/%d, want 1/2", d.nodesUp, d.nodesTotal)
	}
	if d.online != 1 {
		t.Errorf("online = %d, want 1", d.online)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
