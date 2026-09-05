package probe

import (
	"bufio"
	"encoding/json"
	"math/rand"
	"net"
	"os"
	"path/filepath"
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
	// drop is asked about every datagram, told which way it was going. Returning
	// true loses it.
	drop func(toRight bool) bool
	// seen counts what survived, by direction. It is how a test checks that a
	// relay put the count of the leg after on the wire and not the product of
	// both legs' counts.
	seen [2]atomic.Int64
}

func newWire(t *testing.T, right *net.UDPAddr, drop func(bool) bool) (*wire, *net.UDPAddr) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	w := &wire{conn: conn, right: right, drop: drop}
	t.Cleanup(func() { conn.Close() })
	go w.run()
	return w, conn.LocalAddr().(*net.UDPAddr)
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
		if toRight {
			w.seen[0].Add(1)
		} else {
			w.seen[1].Add(1)
		}
		w.conn.WriteToUDP(buf[:n], to)
	}
}

func key() string { return tunnel.EncodeKey(tunnel.NewKey()) }

func mustNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	n.Start()
	return n
}

// bind gives a responder a real port before its config is written, since the
// originator has to be told where to dial.
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

// fast is a config tuned so a test finishes in a second rather than a minute.
func fast(name, logPath string) Config {
	return Config{
		Name: name, Hz: 400, Windows: []string{"200ms"},
		TimeoutMS: 150, Log: logPath, MaxLogMB: 8,
	}
}

func logPath(t *testing.T) string { return filepath.Join(t.TempDir(), "probe.jsonl") }

// read returns every record written so far.
func read(t *testing.T, path string) []line {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []line
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("bad log line %q: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	return out
}

// await polls for records passing want, so a test never depends on how many
// windows happened to close.
func await(t *testing.T, path string, want func([]line) bool) []line {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ls := read(t, path); want(ls) {
			return ls
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting on %s; have %d lines", path, len(read(t, path)))
	return nil
}

// find returns the first record matching want. Tests never index the log by
// position: the first window a node writes is a partial one, opened before the
// probes started, so how many samples it holds is a race with startup.
func find(ls []line, want func(line) bool) (line, bool) {
	for _, l := range ls {
		if want(l) {
			return l, true
		}
	}
	return line{}, false
}

// awaitOne blocks until a record matching want is written, and returns it.
func awaitOne(t *testing.T, path string, want func(line) bool) line {
	t.Helper()
	var got line
	await(t, path, func(ls []line) bool {
		var ok bool
		got, ok = find(ls, want)
		return ok
	})
	return got
}

// leg stands up an originator and a responder over one leg, optionally lossy,
// with one class per duplication count given.
func leg(t *testing.T, drop func(bool) bool, dups ...int) (string, *wire) {
	t.Helper()
	k := key()
	bindAddr, respAddr := bind(t)
	w, wireAddr := newWire(t, respAddr, drop)

	resp := Config{Name: "resp", Bind: bindAddr, Hz: 400, Windows: []string{"200ms"},
		TimeoutMS: 150, Links: []LinkConfig{{Addr: "127.0.0.1", Key: k}}}
	origLog := logPath(t)
	orig := fast("orig", origLog)
	orig.Links = []LinkConfig{{Addr: wireAddr.String(), Key: k}}
	for i, d := range dups {
		id := uint8(i + 1)
		resp.Classes = append(resp.Classes, Class{ID: id, Name: "a>b", Kind: KindLeg, Duplicate: d, Up: []Hop{{Link: 0}}})
		orig.Classes = append(orig.Classes, Class{ID: id, Name: "a>b", Kind: KindLeg, Duplicate: d, Down: []Hop{{Link: 0}}})
	}
	mustNode(t, resp)
	mustNode(t, orig)
	return origLog, w
}

func TestLegMeasuresACleanPath(t *testing.T) {
	path, _ := leg(t, nil, 1)
	// The first window of a series has no directional split and cannot: the count
	// of probes that arrived is the difference of two of the responder's counters,
	// and the earlier one comes from the window before.
	l := awaitOne(t, path, func(l line) bool { return l.N > 10 && l.LossFwd != nil })
	if l.Class != "a>b" || l.Kind != KindLeg || l.Dup != 1 || l.W != "200ms" {
		t.Fatalf("wrong labelling: %+v", l)
	}
	if l.Loss != 0 {
		t.Errorf("loss on a clean leg: %v%%", l.Loss)
	}
	if l.Got != l.Sent {
		t.Errorf("sent %d, got %d on a clean leg", l.Sent, l.Got)
	}
	// A loopback round trip is sub-millisecond, but the ordering has to hold
	// whatever the machine is doing.
	if !(l.Min <= l.P50 && l.P50 <= l.P90 && l.P90 <= l.P99 && l.P99 <= l.Max) {
		t.Errorf("percentiles out of order: %+v", l)
	}
	if l.LossFwd == nil || l.LossRev == nil {
		t.Fatalf("a clean leg should know both directions: %+v", l)
	}
	if *l.LossFwd != 0 || *l.LossRev != 0 {
		t.Errorf("directional loss on a clean leg: fwd=%v rev=%v", *l.LossFwd, *l.LossRev)
	}
}

// TestDuplicationBeatsLoss is the measurement the whole tool exists to make: two
// classes over one leg differing only in how many copies they put on it, and the
// gap between their loss is what duplication buys.
func TestDuplicationBeatsLoss(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	drop := func(bool) bool {
		// 40% independent loss in both directions. Two copies of a probe are lost
		// together only 16% of the time, and a round trip needs both halves, so
		// the two classes must land far apart.
		return rng.Float64() < 0.4
	}
	path, _ := leg(t, drop, 1, 3)

	ls := await(t, path, func(ls []line) bool {
		_, one := find(ls, func(l line) bool { return l.N >= 10 && l.Dup == 1 })
		_, three := find(ls, func(l line) bool { return l.N >= 10 && l.Dup == 3 })
		return one && three
	})
	single, _ := find(ls, func(l line) bool { return l.N >= 10 && l.Dup == 1 })
	triple, _ := find(ls, func(l line) bool { return l.N >= 10 && l.Dup == 3 })
	if single.Loss <= triple.Loss {
		t.Fatalf("duplication bought nothing: dup=1 lost %v%%, dup=3 lost %v%%", single.Loss, triple.Loss)
	}
	if single.Loss < 20 {
		t.Errorf("dup=1 over a 40%% lossy leg should lose a lot, lost %v%%", single.Loss)
	}
	t.Logf("dup=1 loss %.1f%%, dup=3 loss %.1f%%", single.Loss, triple.Loss)
}

// TestLossIsSplitByDirection checks the counters the answer carries: with only the
// outbound direction dropping, the round trip must be attributed to the way out
// and not to the way back.
func TestLossIsSplitByDirection(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	drop := func(toRight bool) bool { return toRight && rng.Float64() < 0.5 }
	path, _ := leg(t, drop, 1)

	ls := await(t, path, func(ls []line) bool {
		_, ok := find(ls, func(l line) bool { return l.N > 10 && l.LossFwd != nil && l.LossRev != nil })
		return ok
	})

	var got bool
	for _, l := range ls {
		if l.N <= 10 || l.LossFwd == nil || l.LossRev == nil {
			continue
		}
		got = true
		if *l.LossFwd < 20 {
			t.Errorf("outbound loss reported as %v%% on a leg dropping half of it", *l.LossFwd)
		}
		if *l.LossRev > 15 {
			t.Errorf("blamed the return path for %v%% when only the way out drops", *l.LossRev)
		}
	}
	if !got {
		t.Fatal("no window carried both directions")
	}
}

// TestChainCountsDoNotCompound puts a relay in the middle and checks the invariant
// the tunnel has too: a node drops what it receives to one copy and sends on with
// its own count, so two legs at two copies each is two on the second leg, not four.
// TestChainCountsDoNotCompound puts a relay in the middle and checks the invariant
// the tunnel has too: a node drops what it receives to one copy and sends on with
// its own leg's count. Two legs at two copies each is two datagrams on the second
// leg per probe, not four, so both legs must carry the same number.
func TestChainCountsDoNotCompound(t *testing.T) {
	k1, k2 := key(), key()
	relayBind, relayAddr := bind(t)
	exitBind, exitAddr := bind(t)
	// One wire per leg, so what each leg carried is counted rather than inferred.
	w2, wire2 := newWire(t, exitAddr, nil)
	w1, wire1 := newWire(t, relayAddr, nil)

	mustNode(t, Config{Name: "exit", Bind: exitBind, Hz: 50, Windows: []string{"200ms"}, TimeoutMS: 150,
		Links:   []LinkConfig{{Addr: "127.0.0.1", Key: k2}},
		Classes: []Class{{ID: 1, Name: "a>b>c", Kind: KindChain, Duplicate: 2, Up: []Hop{{Link: 0}}}}})

	mustNode(t, Config{Name: "relay", Bind: relayBind, Hz: 50, Windows: []string{"200ms"}, TimeoutMS: 150,
		Links: []LinkConfig{{Addr: "127.0.0.1", Key: k1}, {Addr: wire2.String(), Key: k2}},
		Classes: []Class{{ID: 1, Name: "a>b>c", Kind: KindChain, Duplicate: 2,
			Up: []Hop{{Link: 0}}, Down: []Hop{{Link: 1}}}}})

	path := logPath(t)
	entry := fast("entry", path)
	entry.Hz = 50
	entry.Links = []LinkConfig{{Addr: wire1.String(), Key: k1}}
	entry.Classes = []Class{{ID: 1, Name: "a>b>c", Kind: KindChain, Duplicate: 2, Down: []Hop{{Link: 0}}}}
	mustNode(t, entry)

	l := awaitOne(t, path, func(l line) bool { return l.N >= 5 })
	if l.Loss != 0 {
		t.Errorf("loss on a clean chain: %v%%", l.Loss)
	}

	// Both legs are configured for two copies, so both must carry the same count.
	// A relay that forwarded every copy it received would put twice as many on the
	// second leg as it was fed on the first.
	first, second := w1.seen[0].Load(), w2.seen[0].Load()
	if first < 10 {
		t.Fatalf("only %d datagrams on the first leg; nothing was measured", first)
	}
	if float64(second) > 1.4*float64(first) {
		t.Errorf("first leg carried %d datagrams, second %d: counts compounded", first, second)
	}
	if float64(second) < 0.6*float64(first) {
		t.Errorf("first leg carried %d datagrams, second only %d", first, second)
	}
	// And the way back is duplicated too, so the relay is not silently dropping
	// the second copy of an answer without forwarding the first.
	if w1.seen[1].Load() < 10 {
		t.Errorf("only %d answers came back up the first leg", w1.seen[1].Load())
	}
}

func TestSeqWindowTellsCopiesFromNewProbes(t *testing.T) {
	var w seqWindow
	if !w.accept(100) {
		t.Fatal("first sequence should be new")
	}
	if w.accept(100) {
		t.Error("a second copy of 100 was called new")
	}
	if !w.accept(101) || !w.accept(99) {
		t.Error("101 and a late 99 are both new")
	}
	if w.accept(99) {
		t.Error("a second copy of 99 was called new")
	}
	if !w.accept(100000) {
		t.Error("a jump forward is new")
	}
	if w.accept(100000) {
		t.Error("a copy after a jump forward was called new")
	}
	// Far past the window there is no memory, and being wrong the safe way means
	// calling it a copy: counting a probe twice would understate loss.
	if w.accept(100000 - seqWindowSize - 1) {
		t.Error("a sequence older than the window should be treated as a copy")
	}
}

func TestRejectsAConfigItCannotServe(t *testing.T) {
	base := func() Config {
		return Config{Name: "n", Links: []LinkConfig{{Addr: "127.0.0.1:1", Key: key()}},
			Classes: []Class{{ID: 1, Name: "a>b", Duplicate: 1, Down: []Hop{{Link: 0}}}}}
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{"class id zero", func(c *Config) { c.Classes[0].ID = 0 }},
		{"duplicate class id", func(c *Config) { c.Classes = append(c.Classes, c.Classes[0]) }},
		{"link out of range", func(c *Config) { c.Classes[0].Down = []Hop{{Link: 7}} }},
		{"class reaches nothing", func(c *Config) { c.Classes[0].Down = nil }},
		{"answers with no bind", func(c *Config) { c.Classes[0].Down, c.Classes[0].Up = nil, []Hop{{Link: 0}} }},
		{"no classes", func(c *Config) { c.Classes = nil }},
		{"bad key", func(c *Config) { c.Links[0].Key = "nope" }},
		{"bad address", func(c *Config) { c.Links[0].Addr = "not an address" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mut(&c)
			if _, err := New(c); err == nil {
				t.Fatal("accepted a config it cannot serve")
			}
		})
	}
}

func TestRejectsAWindowShorterThanTheProbeInterval(t *testing.T) {
	c := Config{Name: "n", Hz: 1, Windows: []string{"100ms"},
		Links:   []LinkConfig{{Addr: "127.0.0.1:1", Key: key()}},
		Classes: []Class{{ID: 1, Name: "a>b", Duplicate: 1, Down: []Hop{{Link: 0}}}}}
	if _, err := New(c); err == nil {
		t.Fatal("accepted a window that cannot hold one probe")
	}
}
