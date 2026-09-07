package trial

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/tunnel"
)

// wire is a UDP relay between whoever dials it and one fixed address, dropping
// datagrams on demand. Loss is injected on a real socket rather than through a
// seam cut into the code, so what the test exercises is what ships.
type wire struct {
	conn  *net.UDPConn
	right *net.UDPAddr
	left  atomic.Pointer[net.UDPAddr]
	drop  func(toRight bool) bool
}

func newWire(t *testing.T, right *net.UDPAddr, drop func(bool) bool) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	w := &wire{conn: conn, right: right, drop: drop}
	t.Cleanup(func() { conn.Close() })
	go w.run()
	return conn.LocalAddr().(*net.UDPAddr)
}

func (w *wire) run() {
	buf := make([]byte, 65535)
	for {
		n, from, err := w.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		to := w.right
		toRight := true
		if from.String() == w.right.String() {
			toRight = false
			if to = w.left.Load(); to == nil {
				continue
			}
		} else {
			w.left.Store(from)
		}
		if w.drop != nil && w.drop(toRight) {
			continue
		}
		w.conn.WriteToUDP(buf[:n], to)
	}
}

func key() string { return tunnel.EncodeKey(tunnel.NewKey()) }

// bind gives a node a real port before its config is written, since its peers
// have to be told where to dial before any of them exist.
func bind(t *testing.T) (string, *net.UDPAddr) {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	a := c.LocalAddr().(*net.UDPAddr)
	c.Close()
	return a.String(), a
}

func newNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n
}

func mustNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	n := newNode(t, cfg)
	n.Start()
	return n
}

func logPath(t *testing.T, name string) string {
	return filepath.Join(t.TempDir(), name+".jsonl")
}

// rec is every record kind flattened, because a test reads one file holding all
// three and cares about a different handful of fields in each.
type rec struct {
	K       string   `json:"k"`
	Leg     string   `json:"leg"`
	Tick    uint64   `json:"tick"`
	Path    string   `json:"path"`
	Onward  int      `json:"onward"`
	W       string   `json:"w"`
	N       int      `json:"n"`
	Sent    int      `json:"sent"`
	Got     int      `json:"got"`
	Fwd     *int     `json:"fwd"`
	Loss    float64  `json:"loss"`
	LossFwd *float64 `json:"loss_fwd"`
	LossRev *float64 `json:"loss_rev"`
	P50     float64  `json:"p50"`
	Expect  float64  `json:"expect"`
	Hops    string   `json:"-"`
	Err     string   `json:"err"`
}

func read(t *testing.T, path string) []rec {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []rec
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var r rec
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad log line %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

// await polls for records passing want, so a test never depends on how many
// windows or ticks happened to land while it was looking.
func await(t *testing.T, path string, what string, want func([]rec) bool) []rec {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if rs := read(t, path); want(rs) {
			return rs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s in %s; have %d records", what, path, len(read(t, path)))
	return nil
}

// mesh builds the real thing: the five nodes of the Tokyo trial, wired with the
// seven edges the flood runs over, all on loopback. Nothing here is a
// simplification of the deployed shape — that is the point, since what is being
// checked is that the shape produces the paths it is supposed to.
type mesh struct {
	logs map[string]string
	node map[string]*Node
}

func newMesh(t *testing.T, hz float64, windows []string, timeoutMS int) *mesh {
	t.Helper()

	ids := map[string]uint8{"hk": 1, "ty-a": 2, "ty-b": 3, "ty-c": 4, "ch": 5}
	edges := [][2]string{
		{"hk", "ty-a"}, {"hk", "ty-b"},
		{"ty-a", "ty-b"}, {"ty-a", "ty-c"},
		{"ty-b", "ch"}, {"ty-b", "ty-c"},
		{"ty-c", "ch"},
	}
	// One key per undirected pair, held by both ends: which leg a datagram
	// belongs to is decided by which key opens it.
	keys := map[string]string{}
	for _, e := range edges {
		keys[e[0]+"|"+e[1]] = key()
	}
	keyFor := func(a, b string) string {
		if k, ok := keys[a+"|"+b]; ok {
			return k
		}
		return keys[b+"|"+a]
	}
	neighbours := func(name string) []string {
		var out []string
		for _, e := range edges {
			switch name {
			case e[0]:
				out = append(out, e[1])
			case e[1]:
				out = append(out, e[0])
			}
		}
		return out
	}

	addrs := map[string]string{}
	for name := range ids {
		addrs[name], _ = bind(t)
	}

	// The forward flood: hk to ch, every out-edge, loop-guarded. ch lists no run
	// and is therefore the terminus, which is a fact about its config and not a
	// flag anybody set.
	forward := map[string][]string{
		"hk":        {"ty-a", "ty-b"},
		"ty-a":     {"ty-b", "ty-c"},
		"ty-b": {"ch", "ty-c"},
		"ty-c":    {"ch"},
	}

	m := &mesh{logs: map[string]string{}, node: map[string]*Node{}}
	for name, id := range ids {
		cfg := Config{
			Name: name, ID: id, Bind: addrs[name],
			Hz: hz, Windows: windows, TimeoutMS: timeoutMS,
			Log: logPath(t, name), MaxLogMB: 8,
			Names: ids,
		}
		for _, p := range neighbours(name) {
			cfg.Legs = append(cfg.Legs, Leg{
				Peer: p, PeerID: ids[p], Addr: addrs[p], Key: keyFor(name, p), ExpectMS: 1,
			})
		}
		if fwd, ok := forward[name]; ok {
			cfg.Runs = []Run{{From: "hk", FromID: ids["hk"], Forward: fwd}}
		}
		m.logs[name] = cfg.Log
		m.node[name] = newNode(t, cfg)
	}
	for _, n := range m.node {
		n.Start()
	}
	return m
}

// The five paths hk reaches ch by, which is what the seven edges enumerate once
// the loop guard has removed everything that would revisit a node.
var wantPaths = []string{
	"hk>ty-b>ch",
	"hk>ty-b>ty-c>ch",
	"hk>ty-a>ty-b>ch",
	"hk>ty-a>ty-b>ty-c>ch",
	"hk>ty-a>ty-c>ch",
}

func arrivals(rs []rec) []rec {
	var out []rec
	for _, r := range rs {
		if r.K == "rx" {
			out = append(out, r)
		}
	}
	return out
}

// A flood puts one copy on every path and the exit keeps all of them. probed
// would collapse these five to one, because a player's packet only has to arrive
// once; here the whole question is which route delivered it first, so a copy
// dropped as a duplicate is the answer being thrown away.
func TestEveryPathArrivesAndNoneAreCollapsed(t *testing.T) {
	m := newMesh(t, 1, []string{"1s"}, 500)

	// Polled here rather than through await so that a failure can say what the
	// exit actually saw. "five copies arrived but one path is missing" and "the
	// mesh is not delivering" are different bugs and the count alone tells them
	// apart from neither.
	var got []rec
	var byTick map[uint64][]string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got = read(t, m.logs["ch"])
		byTick = map[uint64][]string{}
		for _, r := range arrivals(got) {
			byTick[r.Tick] = append(byTick[r.Tick], r.Path)
		}
		for _, paths := range byTick {
			if samePaths(paths, wantPaths) {
				goto found
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	for tick, paths := range byTick {
		sort.Strings(paths)
		t.Logf("tick %d delivered %d copies: %v", tick, len(paths), paths)
	}
	t.Fatalf("no tick was delivered over all five paths; want %v", wantPaths)

found:

	// And every path the exit ever saw is one of the five: a sixth would mean the
	// loop guard let something through, and a missing one that an edge is dead.
	seen := map[string]bool{}
	for _, r := range arrivals(got) {
		seen[r.Path] = true
	}
	for p := range seen {
		if !contains2(wantPaths, p) {
			t.Errorf("exit saw path %q, which is not one the mesh should produce", p)
		}
	}
}

// The loop guard is what makes a flood over a mesh terminate at all: two of these
// nodes have an edge between them and both are downstream of hk, so without it a
// copy would go round until the datagram ran out of room.
func TestNoPathEverRevisitsANode(t *testing.T) {
	m := newMesh(t, 1, []string{"1s"}, 500)

	for _, name := range []string{"ch", "ty-c", "ty-b"} {
		rs := await(t, m.logs[name], "an arrival at "+name, func(rs []rec) bool {
			return len(arrivals(rs)) > 0
		})
		for _, r := range arrivals(rs) {
			hops := strings.Split(r.Path, ">")
			seen := map[string]bool{}
			for _, h := range hops {
				if seen[h] {
					t.Fatalf("%s logged path %q, which visits %s twice", name, r.Path, h)
				}
				seen[h] = true
			}
			if len(hops) > maxPath {
				t.Fatalf("%s logged path %q, longer than the %d-hop cap", name, r.Path, maxPath)
			}
		}
	}
}

// A node in the middle passes a copy on down each of its onward edges and no
// more. Two in, two out each, is four onward from ty-b per tick — not the
// product of the two, and not one after a de-duplication that must not happen.
func TestARelayPassesOnOneCopyPerOnwardEdge(t *testing.T) {
	m := newMesh(t, 1, []string{"1s"}, 500)

	rs := await(t, m.logs["ty-b"], "both copies of one tick at ty-b", func(rs []rec) bool {
		byTick := map[uint64]int{}
		for _, r := range arrivals(rs) {
			byTick[r.Tick]++
		}
		for _, n := range byTick {
			if n == 2 {
				return true
			}
		}
		return false
	})
	for _, r := range arrivals(rs) {
		if r.Onward != 2 {
			t.Errorf("ty-b passed %q on over %d edges, want 2 (ch and ty-c)", r.Path, r.Onward)
		}
	}
}

// Round-trip loss alone cannot say which direction dropped. The echo carries the
// far end's count of what arrived, and differencing that across a window splits
// it — so a leg losing probes on the way out reads differently from one whose
// probes all arrive and whose answers do not come back.
func TestLossOnTheWayOutIsNotLossOnTheWayBack(t *testing.T) {
	var n atomic.Int64
	fwd, rev := twoNodes(t, func(toRight bool) bool {
		if !toRight {
			return false
		}
		return n.Add(1)%3 == 0 // one probe in three never reaches the far end
	})

	r := awaitSplit(t, fwd)
	if *r.LossFwd < 20 || *r.LossFwd > 50 {
		t.Errorf("loss_fwd = %.1f%%, want about a third", *r.LossFwd)
	}
	if *r.LossRev != 0 {
		t.Errorf("loss_rev = %.1f%%, want 0: nothing was dropped coming back", *r.LossRev)
	}
	_ = rev
}

func TestLossOnTheWayBackIsNotLossOnTheWayOut(t *testing.T) {
	var n atomic.Int64
	fwd, _ := twoNodes(t, func(toRight bool) bool {
		if toRight {
			return false
		}
		return n.Add(1)%3 == 0 // every probe arrives; one answer in three does not
	})

	r := awaitSplit(t, fwd)
	// Not exactly zero, and it cannot be. How many probes arrived is learned from
	// a counter the far end carries back on its echoes, so when the echoes are the
	// lossy direction, a window whose last echo was dropped reads a slightly stale
	// count. The residual is a probe or two out of a hundred; what matters is that
	// it stays an order of magnitude below the loss that is really there.
	if *r.LossFwd > 5 {
		t.Errorf("loss_fwd = %.1f%%, want near 0: every probe reached the far end", *r.LossFwd)
	}
	if *r.LossRev < 20 || *r.LossRev > 50 {
		t.Errorf("loss_rev = %.1f%%, want about a third", *r.LossRev)
	}
	if *r.LossFwd >= *r.LossRev {
		t.Errorf("loss_fwd %.1f%% is not below loss_rev %.1f%%; the split says nothing",
			*r.LossFwd, *r.LossRev)
	}
}

// twoNodes is one leg with a lossy wire in the middle. The near end probes it on
// its own, the way a leg no run crosses is measured.
func twoNodes(t *testing.T, drop func(bool) bool) (nearLog, farLog string) {
	t.Helper()
	nearAddr, _ := bind(t)
	farAddr, farUDP := bind(t)
	relay := newWire(t, farUDP, drop)
	k := key()

	near := Config{
		Name: "near", ID: 1, Bind: nearAddr,
		Hz: 100, Windows: []string{"1s"}, TimeoutMS: 200,
		Log: logPath(t, "near"), MaxLogMB: 8,
		Legs: []Leg{{Peer: "far", PeerID: 2, Addr: relay.String(), Key: k, ExpectMS: 1, Echo: true}},
	}
	far := Config{
		Name: "far", ID: 2, Bind: farAddr,
		Hz: 100, Windows: []string{"1s"}, TimeoutMS: 200,
		Log: logPath(t, "far"), MaxLogMB: 8,
		Legs: []Leg{{Peer: "near", PeerID: 1, Addr: relay.String(), Key: k, ExpectMS: 1}},
	}
	mustNode(t, near)
	mustNode(t, far)
	return near.Log, far.Log
}

// awaitSplit waits for a window that can report a direction, which is never the
// first one: the count that arrived is the difference of two of the far end's
// counters and the earlier one comes from the window before.
func awaitSplit(t *testing.T, path string) rec {
	t.Helper()
	var wins []rec
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		wins = wins[:0]
		for _, r := range read(t, path) {
			if r.K != "win" {
				continue
			}
			wins = append(wins, r)
			// Enough samples that a third of them is a third and not a rounding
			// artefact. The bar is low because -race slows the whole thing down.
			if r.LossFwd != nil && r.LossRev != nil && r.Sent >= 12 {
				return r
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, r := range wins {
		t.Logf("win sent=%d got=%d fwd=%v loss=%.1f fwd=%v rev=%v",
			r.Sent, r.Got, deref(r.Fwd), r.Loss, derefF(r.LossFwd), derefF(r.LossRev))
	}
	t.Fatalf("no window in %s ever reported a direction split", path)
	return rec{}
}

func deref(p *int) any {
	if p == nil {
		return "null"
	}
	return *p
}

func derefF(p *float64) any {
	if p == nil {
		return "null"
	}
	return *p
}

// A leg that stays slow is worth one look at the path, not one every window. The
// cooldown starts when the trace is authorised, so a trace that hangs cannot be
// followed straight away by another.
func TestATraceFiresOnceAndThenWaitsOutItsCooldown(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	tr := newTracer(Trace{OverMS: 5, CooldownS: 600, Count: 5, Bin: "mtr", TimeoutS: 40})
	tr.now = func() time.Time { return now }

	if !tr.take("ty-a") {
		t.Fatal("the first look at a leg should be allowed")
	}
	if tr.take("ty-a") {
		t.Error("a second look at the same leg inside the cooldown should not be")
	}
	// The cooldown is per leg: another leg degrading at the same moment is another
	// path to describe, and holding it to one would describe whichever window
	// happened to close first.
	if !tr.take("ty-b") {
		t.Error("a different leg should not be held back by ty-a's cooldown")
	}

	now = now.Add(601 * time.Second)
	if !tr.take("ty-a") {
		t.Error("the cooldown should have expired")
	}
}

// The window is the trigger, not a single probe: a leg whose mdev is a few
// milliseconds would trip a per-sample threshold more or less continuously.
func TestAWindowOverExpectationIsTracedAndRecorded(t *testing.T) {
	addrA, _ := bind(t)
	addrB, _ := bind(t)
	k := key()

	cfg := Config{
		Name: "near", ID: 1, Bind: addrA,
		Hz: 100, Windows: []string{"1s"}, TimeoutMS: 200,
		Log: logPath(t, "near"), MaxLogMB: 8,
		// A margin small enough that loopback clears it, standing in for a leg
		// that has gone 5 ms past what it is supposed to cost.
		Trace: &Trace{OverMS: 0.0001, CooldownS: 600, Count: 2, Bin: "mtr", TimeoutS: 5},
		Legs:  []Leg{{Peer: "far", PeerID: 2, Addr: addrB, Key: k, ExpectMS: 0.0001, Echo: true}},
	}
	far := Config{
		Name: "far", ID: 2, Bind: addrB,
		Hz: 100, Windows: []string{"1s"}, TimeoutMS: 200,
		Log: logPath(t, "far"), MaxLogMB: 8,
		Legs: []Leg{{Peer: "near", PeerID: 1, Addr: addrA, Key: k, ExpectMS: 1}},
	}

	n := newNode(t, cfg)
	var ran atomic.Int64
	var gotArgs atomic.Pointer[[]string]
	n.tr.run = func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		ran.Add(1)
		a := append([]string(nil), args...)
		gotArgs.Store(&a)
		return []byte(`{"report":{"hubs":[]}}`), nil
	}
	n.Start()
	mustNode(t, far)

	await(t, cfg.Log, "a trace record", func(rs []rec) bool {
		for _, r := range rs {
			if r.K == "trace" {
				if r.Err != "" {
					t.Fatalf("trace recorded an error: %s", r.Err)
				}
				return true
			}
		}
		return false
	})

	args := gotArgs.Load()
	if args == nil {
		t.Fatal("the trace ran but recorded no arguments")
	}
	joined := strings.Join(*args, " ")
	// UDP to the port the probe uses, so the five-tuple hashes onto whichever
	// ECMP path the probe itself takes. An ICMP trace describes a path our
	// traffic may never see.
	for _, want := range []string{"--json", "--udp", "-P"} {
		if !strings.Contains(joined, want) {
			t.Errorf("traced with %q, missing %s", joined, want)
		}
	}
}

func samePaths(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	sort.Strings(g)
	for i := range g {
		if g[i] != want[i] {
			return false
		}
	}
	return true
}

func contains2(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
