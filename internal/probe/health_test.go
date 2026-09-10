package probe

import (
	"net"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/tunnel"
)

func healthNode(t *testing.T, allow []string) (*HealthClient, string) {
	t.Helper()
	k := key()
	cfg := fast("hk", logPath(t))
	// One class, dialling a port nothing answers on: the probes time out, which
	// is irrelevant here and keeps the node a legal one.
	cfg.Links = []LinkConfig{{Addr: "127.0.0.1:1", Key: k}}
	cfg.Classes = []Class{{ID: 1, Name: "a>b", Kind: KindLeg, Duplicate: 1, Down: []Hop{{Link: 0}}}}
	cfg.Health = &HealthConfig{Bind: "127.0.0.1:0", Key: k, AllowFrom: allow}

	// Bind ourselves so the test knows the port; New would pick one we cannot see.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfg.Health.Bind = addr

	n := mustNode(t, cfg)
	_ = n
	dk, err := tunnel.DecodeKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return &HealthClient{Addr: addr, Key: dk}, k
}

func TestHealthAnswersThatProbedIsRunning(t *testing.T) {
	c, _ := healthNode(t, nil)
	h, err := c.Check()
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if h.Node != "hk" {
		t.Errorf("node = %q, want hk", h.Node)
	}
	// The count is here so that "running" and "running but measuring nothing"
	// can be told apart, which a bare TCP connect cannot do.
	if h.Classes != 1 {
		t.Errorf("classes = %d, want 1", h.Classes)
	}
	if h.UpSecs < 0 {
		t.Errorf("up = %ds", h.UpSecs)
	}
}

// Wrong key gets nothing back at all, exactly as the control link gives nothing
// back: a refusal would tell a guesser that something is listening here.
func TestHealthRefusesTheWrongKey(t *testing.T) {
	c, _ := healthNode(t, nil)
	c.Key = tunnel.NewKey()
	if _, err := c.Check(); err == nil {
		t.Fatal("a wrong key was answered")
	}
}

// Loopback always may. Anything else has to be named, and a test that runs over
// loopback cannot prove the refusal — so check the predicate directly.
func TestHealthAllowsLoopbackAndNamedAddressesOnly(t *testing.T) {
	h := &healthServer{}
	for _, a := range []string{"198.51.100.10"} {
		p, err := parsePrefix(a)
		if err != nil {
			t.Fatal(err)
		}
		h.allow = append(h.allow, p)
	}
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"198.51.100.10", true},
		{"198.51.100.11", false},
	} {
		got := h.allowed(&net.TCPAddr{IP: net.ParseIP(tc.ip)})
		if got != tc.want {
			t.Errorf("allowed(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// A class that has not closed a window yet is still listed, with negative
// figures. The bot needs the difference between that and a node that originates
// nothing: the first has a latency coming, the second never will.
func TestHealthListsAClassBeforeItHasMeasuredAnything(t *testing.T) {
	c, _ := healthNode(t, nil)
	h, err := c.Check()
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(h.Legs) != 1 {
		t.Fatalf("legs = %v, want the one class configured", h.Legs)
	}
	if h.Legs[0].Class != "a>b" {
		t.Errorf("class = %q, want a>b", h.Legs[0].Class)
	}
	if h.Legs[0].P50 >= 0 || h.Legs[0].N >= 0 || h.Legs[0].AgeSecs >= 0 {
		t.Errorf("a class with no closed window reported figures: %+v", h.Legs[0])
	}
}

// Once a window has closed, the class it belongs to is on the health answer with
// its age, which is the whole reason the link carries it.
func TestHealthReportsTheNewestWindow(t *testing.T) {
	n := &Node{
		short:   time.Minute,
		recent:  map[string]*Report{},
		classes: map[uint8]*class{1: {Class: Class{Name: "a>b", Kind: KindLeg}, down: []hop{{}}}},
	}
	n.keep(&Report{
		At: time.Now().Add(-90 * time.Second), Window: time.Minute,
		Class: "a>b", Kind: KindLeg, N: 60, Sent: 60, Got: 60, Fwd: 60, P50: 12.5,
	})
	legs := n.Legs()
	if len(legs) != 1 {
		t.Fatalf("legs = %v, want one", legs)
	}
	if legs[0].Class != "a>b" || legs[0].P50 != 12.5 || legs[0].Window != "1m" {
		t.Errorf("leg = %+v", legs[0])
	}
	// The window closed thirty seconds ago: it ran from 90s ago to 30s ago.
	if legs[0].AgeSecs < 25 || legs[0].AgeSecs > 35 {
		t.Errorf("age = %ds, want about 30", legs[0].AgeSecs)
	}
	if legs[0].Loss != 0 {
		t.Errorf("loss = %v, want 0", legs[0].Loss)
	}
}
