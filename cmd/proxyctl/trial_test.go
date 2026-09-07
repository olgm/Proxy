package main

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/olgm/proxy/internal/trial"
)

func testMesh() *TrialMesh {
	return &TrialMesh{
		Port: 9400,
		Nodes: map[string]TrialNode{
			"hk":        {SSH: "root@hk", Addr: "10.0.0.1", ID: 1},
			"ty-a":     {SSH: "root@v", Addr: "10.0.0.2", ID: 2},
			"ty-b": {SSH: "root@h", Addr: "10.0.0.3", ID: 3},
			"ty-c":    {SSH: "root@l", Addr: "10.0.0.4", ID: 4},
			"ch":        {SSH: "root@c", Addr: "10.0.0.5", ID: 5},
			"ty-d":    {SSH: "root@m", Addr: "10.0.0.6", ID: 6},
		},
		Legs: []TrialLeg{
			{A: "hk", B: "ty-a", ExpectMS: 45},
			{A: "hk", B: "ty-b", ExpectMS: 70},
			{A: "ty-a", B: "ty-b", ExpectMS: 1},
			{A: "ty-a", B: "ty-c", ExpectMS: 1},
			{A: "ty-b", B: "ty-c", ExpectMS: 1},
			{A: "ty-b", B: "ch", ExpectMS: 128},
			{A: "ty-c", B: "ch", ExpectMS: 130},
			{A: "ty-a", B: "ty-d", ExpectMS: 1},
		},
		Runs: []TrialRun{
			{From: "hk", Edges: []string{
				"hk>ty-a", "hk>ty-b", "ty-a>ty-b", "ty-a>ty-c",
				"ty-b>ch", "ty-b>ty-c", "ty-c>ch",
			}},
			{From: "ch", ReverseOf: "hk"},
		},
	}
}

func testKeys(t *testing.T) *keyring {
	t.Helper()
	k, err := loadKeys(filepath.Join(t.TempDir(), "trial.json"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func expandMesh(t *testing.T, m *TrialMesh) map[string]*trial.Config {
	t.Helper()
	cfgs, err := expandTrial(m, testKeys(t))
	if err != nil {
		t.Fatal(err)
	}
	return cfgs
}

func runOf(c *trial.Config, from string) *trial.Run {
	for i := range c.Runs {
		if c.Runs[i].From == from {
			return &c.Runs[i]
		}
	}
	return nil
}

func legOf(c *trial.Config, peer string) *trial.Leg {
	for i := range c.Legs {
		if c.Legs[i].Peer == peer {
			return &c.Legs[i]
		}
	}
	return nil
}

// The return direction is the forward run's edges turned around, rather than a
// second list to keep in step with the first. A mesh where one edge was reversed
// and another was not would measure a path nobody asked about and nothing would
// say so.
func TestTheReverseRunIsTheForwardRunTurnedAround(t *testing.T) {
	cfgs := expandMesh(t, testMesh())

	want := map[string][]string{
		"ch":        {"ty-b", "ty-c"},
		"ty-c":    {"ty-b", "ty-a"},
		"ty-b": {"hk", "ty-a"},
		"ty-a":     {"hk"},
	}
	for node, fwd := range want {
		r := runOf(cfgs[node], "ch")
		if r == nil {
			t.Fatalf("%s has no part in the run from ch", node)
		}
		got := append([]string(nil), r.Forward...)
		sort.Strings(got)
		if len(got) != len(fwd) {
			t.Fatalf("%s forwards ch's run to %v, want %v", node, got, fwd)
		}
		for i := range got {
			if got[i] != fwd[i] {
				t.Fatalf("%s forwards ch's run to %v, want %v", node, got, fwd)
			}
		}
	}
}

// A run's origin is the terminus of the run coming back, and neither is declared:
// hk lists no forwarding for ch's run, so it holds what arrives. That is the same
// "roles are emergent" rule proxyd and probed follow.
func TestAnOriginIsTheOtherRunsTerminus(t *testing.T) {
	cfgs := expandMesh(t, testMesh())

	if r := runOf(cfgs["hk"], "hk"); r == nil || len(r.Forward) != 2 {
		t.Errorf("hk should start its own run over two legs, got %v", r)
	}
	if r := runOf(cfgs["hk"], "ch"); r != nil {
		t.Errorf("hk forwards ch's run to %v; it should hold it", r.Forward)
	}
	if r := runOf(cfgs["ch"], "hk"); r != nil {
		t.Errorf("ch forwards hk's run to %v; it should hold it", r.Forward)
	}
	if r := runOf(cfgs["ch"], "ch"); r == nil || len(r.Forward) != 2 {
		t.Errorf("ch should start its own run over two legs, got %v", r)
	}
}

// A leg a run crosses is measured by that run's own traffic, which is the whole
// reason the mesh is built this way. Only a leg no run touches needs traffic of
// its own, and working that out here rather than writing it down is what stops
// the two disagreeing.
func TestOnlyALegNoRunCrossesIsProbedOnItsOwn(t *testing.T) {
	cfgs := expandMesh(t, testMesh())

	if l := legOf(cfgs["ty-a"], "ty-d"); l == nil || !l.Echo {
		t.Error("ty-a>ty-d is on no run and should be probed on its own")
	}
	if l := legOf(cfgs["ty-d"], "ty-a"); l == nil || !l.Echo {
		t.Error("ty-d>ty-a is on no run and should be probed on its own")
	}
	for _, node := range []string{"hk", "ty-a", "ty-b", "ty-c", "ch"} {
		for _, l := range cfgs[node].Legs {
			if l.Peer == "ty-d" {
				continue
			}
			if l.Echo {
				t.Errorf("%s>%s is crossed by a run and should not also be probed on its own",
					node, l.Peer)
			}
		}
	}
}

// Both ends of a leg have to hold the same key, since that key is what decides
// which leg a datagram belongs to. They are minted per pair and not per direction
// so this cannot come out otherwise.
func TestBothEndsOfALegHoldOneKey(t *testing.T) {
	cfgs := expandMesh(t, testMesh())

	for _, node := range sortedAggs(cfgs) {
		for _, l := range cfgs[node].Legs {
			back := legOf(cfgs[l.Peer], node)
			if back == nil {
				t.Fatalf("%s has a leg to %s but not the other way round", node, l.Peer)
			}
			if back.Key != l.Key {
				t.Errorf("%s and %s hold different keys for the leg between them", node, l.Peer)
			}
		}
	}
}

// Every node gets the whole roster, because the node that has to spell a route is
// the exit, and the exit has no leg to the nodes in the middle of it.
func TestEveryNodeCanSpellEveryOtherNode(t *testing.T) {
	cfgs := expandMesh(t, testMesh())

	for _, node := range sortedAggs(cfgs) {
		if got, want := len(cfgs[node].Names), 6; got != want {
			t.Errorf("%s knows %d node names, want all %d", node, got, want)
		}
		if cfgs[node].Names["hk"] != 1 {
			t.Errorf("%s cannot name hk, so it would log a path as bare ids", node)
		}
	}
}

// An edge naming a pair that is not a declared leg is a mesh with a hole in it,
// and would otherwise deploy as a node quietly forwarding nowhere.
func TestARunOverAnUndeclaredLegIsRejected(t *testing.T) {
	m := testMesh()
	m.Runs[0].Edges = append(m.Runs[0].Edges, "hk>ty-c")
	if _, err := expandTrial(m, testKeys(t)); err == nil {
		t.Fatal("an edge with no leg behind it should not expand")
	}
}

func TestReverseOfSomethingThatIsNotARunIsRejected(t *testing.T) {
	m := testMesh()
	m.Runs[1].ReverseOf = "nowhere"
	if _, err := expandTrial(m, testKeys(t)); err == nil {
		t.Fatal("reverse_of naming no run should not expand")
	}
}

// The generated config has to satisfy triald's own validation, or the deploy
// succeeds and the service fails to start on every node at once.
func TestWhatIsGeneratedIsWhatTrialdAccepts(t *testing.T) {
	for node, cfg := range expandMesh(t, testMesh()) {
		if err := trial.Validate(*cfg); err != nil {
			t.Errorf("%s: triald would reject its own config: %v", node, err)
		}
	}
}

// A node that starts a run cannot also probe a leg on its own: a probe says which
// run it belongs to by whose id opens its path, so its peers could not tell the
// two apart and would forward the bare one. Adding a leg to hk that no run crosses
// is how somebody would walk into that, and expand has to stop it here rather than
// let six nodes find out.
func TestAnOriginCannotAlsoProbeALegOnItsOwn(t *testing.T) {
	m := testMesh()
	m.Legs = append(m.Legs, TrialLeg{A: "hk", B: "ty-d", ExpectMS: 50})
	_, err := expandTrial(m, testKeys(t))
	if err == nil {
		t.Fatal("hk starts a run and would have got an echo leg; that should not expand")
	}
	if !strings.Contains(err.Error(), "echo") {
		t.Errorf("error should say what is wrong with the leg, got: %v", err)
	}
}
