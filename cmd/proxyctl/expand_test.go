package main

import (
	"testing"

	"github.com/olgm/proxy/internal/proxy"
)

func topo(routes ...Route) *Topology {
	return &Topology{
		User:     "proxyd",
		BasePort: 9000,
		Nodes: map[string]Node{
			"hk":  {SSH: "root@hk", Addr: "198.51.100.10"},
			"ty":  {SSH: "root@ty", Addr: "198.51.100.20"},
			"sg":  {SSH: "root@sg", Addr: "198.51.100.40"},
			"chi": {SSH: "root@chi", Addr: "198.51.100.30"},
		},
		Routes: routes,
		keys:   &keyring{keys: map[string]string{}},
	}
}

func hypixel() Target {
	return Target{Addr: "mc.hypixel.net:25565", RewriteHost: "mc.hypixel.net", RewritePort: 25565}
}

func expandOK(t *testing.T, top *Topology) (map[string]*proxy.Config, []check) {
	t.Helper()
	for i := range top.Routes {
		if err := top.Routes[i].normalize(); err != nil {
			t.Fatalf("normalize: %v", err)
		}
	}
	cfgs, checks, err := expand(top)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	return cfgs, checks
}

func only1(t *testing.T, cfgs map[string]*proxy.Config, node string) proxy.Listener {
	t.Helper()
	c, ok := cfgs[node]
	if !ok {
		t.Fatalf("no config for %q", node)
	}
	if len(c.Listeners) != 1 {
		t.Fatalf("%s: %d listeners, want 1", node, len(c.Listeners))
	}
	return c.Listeners[0]
}

// The plain chain has to keep expanding exactly as it did before UDP existed.
func TestExpandTCPUnchanged(t *testing.T) {
	top := topo(Route{Name: "hypixel", Entry: "hk", Port: 25565,
		Via: []string{"ty", "chi"}, Target: hypixel()})
	cfgs, _ := expandOK(t, top)

	hk := only1(t, cfgs, "hk")
	if hk.Bind != ":25565" || hk.Upstream != "198.51.100.20:9000" || hk.Net != "" {
		t.Errorf("hk: %+v", hk)
	}
	if hk.Minecraft == nil || hk.Minecraft.RewriteHost != "mc.hypixel.net" {
		t.Errorf("hk lost its minecraft block: %+v", hk.Minecraft)
	}
	ty := only1(t, cfgs, "ty")
	if ty.Bind != ":9000" || ty.Upstream != "198.51.100.30:9001" ||
		len(ty.AllowFrom) != 1 || ty.AllowFrom[0] != "198.51.100.10" {
		t.Errorf("ty: %+v", ty)
	}
	chi := only1(t, cfgs, "chi")
	if chi.Bind != ":9001" || chi.Upstream != "mc.hypixel.net:25565" ||
		chi.AllowFrom[0] != "198.51.100.20" {
		t.Errorf("chi: %+v", chi)
	}
}

func TestExpandUDPSinglePath(t *testing.T) {
	top := topo(Route{Name: "hypixel", Entry: "hk", Port: 25565, Transport: "udp",
		Via: []string{"ty", "chi"}, Target: hypixel()})
	cfgs, checks := expandOK(t, top)

	hk := only1(t, cfgs, "hk")
	if hk.Net != "" {
		t.Errorf("the entry must stay a tcp listener, got %q", hk.Net)
	}
	if hk.Upstream != "" || len(hk.Hops) != 1 {
		t.Fatalf("hk: %+v", hk)
	}
	if hk.Hops[0].Addr != "198.51.100.20:9000" || hk.Hops[0].Duplicate != defaultDuplicate {
		t.Errorf("hk hop: %+v", hk.Hops[0])
	}

	ty := only1(t, cfgs, "ty")
	if ty.Net != "udp" || ty.Bind != ":9000" || len(ty.Peers) != 1 || len(ty.Hops) != 1 {
		t.Fatalf("ty: %+v", ty)
	}
	if ty.Peers[0].Addr != "198.51.100.10" {
		t.Errorf("a peer is matched on address only, got %q", ty.Peers[0].Addr)
	}
	// A relay drops what arrives to one copy and sends on with its own count,
	// on both legs and in both directions.
	if ty.Peers[0].Duplicate != defaultDuplicate || ty.Hops[0].Duplicate != defaultDuplicate {
		t.Errorf("relay lacks a count for one of its legs: %+v %+v", ty.Peers[0], ty.Hops[0])
	}

	chi := only1(t, cfgs, "chi")
	if chi.Net != "udp" || chi.Bind != ":9001" || chi.Upstream != "mc.hypixel.net:25565" ||
		len(chi.Hops) != 0 || len(chi.Peers) != 1 {
		t.Fatalf("chi: %+v", chi)
	}
	if chi.Peers[0].Duplicate != defaultDuplicate {
		t.Errorf("the exit duplicates the way back too, got %d", chi.Peers[0].Duplicate)
	}

	// Both ends of a leg must hold the same key, or nothing decrypts.
	if hk.Hops[0].Key != ty.Peers[0].Key {
		t.Error("hk and ty disagree about the key for their leg")
	}
	if ty.Hops[0].Key != chi.Peers[0].Key {
		t.Error("ty and chi disagree about the key for their leg")
	}
	if hk.Hops[0].Key == ty.Hops[0].Key {
		t.Error("two legs share one key")
	}

	// One public TCP check, plus one UDP check per leg, from the side that dials.
	if len(checks) != 3 {
		t.Fatalf("checks: %+v", checks)
	}
	if checks[0].udp || checks[0].port != 25565 {
		t.Errorf("entry check: %+v", checks[0])
	}
	for _, c := range checks[1:] {
		if !c.udp {
			t.Errorf("hop check is not udp: %+v", c)
		}
	}
}

func TestExpandUDPRace(t *testing.T) {
	top := topo(Route{Name: "hypixel", Entry: "hk", Port: 25565, Transport: "udp",
		Exit: "chi", Target: hypixel(),
		Paths: []Path{
			{Via: []string{"ty"}},
			{Via: nil, Duplicate: 1},
		}})
	cfgs, checks := expandOK(t, top)

	hk := only1(t, cfgs, "hk")
	if len(hk.Hops) != 2 {
		t.Fatalf("hk should race two paths: %+v", hk.Hops)
	}
	if hk.Hops[0].Addr != "198.51.100.20:9000" || hk.Hops[0].Duplicate != 2 {
		t.Errorf("relayed path: %+v", hk.Hops[0])
	}
	if hk.Hops[1].Addr != "198.51.100.30:9001" || hk.Hops[1].Duplicate != 1 {
		t.Errorf("direct path: %+v", hk.Hops[1])
	}

	chi := only1(t, cfgs, "chi")
	if len(chi.Peers) != 2 {
		t.Fatalf("the exit should answer both paths: %+v", chi.Peers)
	}
	dup := map[string]int{}
	for _, p := range chi.Peers {
		dup[p.Addr] = p.Duplicate
	}
	if dup["198.51.100.20"] != 2 || dup["198.51.100.10"] != 1 {
		t.Errorf("the way back does not mirror the paths: %v", dup)
	}
	// One entry check plus one per leg: hk->ty, hk->chi, ty->chi.
	if len(checks) != 4 {
		t.Fatalf("checks: %+v", checks)
	}
}

// A leg shared by two paths carries one stream of packets, so it cannot be asked
// for two different copy counts.
func TestConflictingDuplicateRejected(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 1, Transport: "udp", Exit: "chi", Target: hypixel(),
		Paths: []Path{
			{Via: []string{"ty"}, Duplicate: 2},
			{Via: []string{"ty", "sg"}, Duplicate: 3},
		}})
	if err := top.Routes[0].normalize(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := expand(top); err == nil {
		t.Fatal("accepted two copy counts on one leg")
	}
}

// One leg can carry a different count from the rest of its path, and both ends
// of it are told: the node sending into it and the node sending back across it.
func TestLegOverridesPath(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Transport: "udp",
		Via: []string{"ty", "chi"}, Target: hypixel(),
		Legs: []Leg{{From: "ty", To: "chi", Duplicate: 1}}})
	cfgs, _ := expandOK(t, top)
	hk, ty, chi := only1(t, cfgs, "hk"), only1(t, cfgs, "ty"), only1(t, cfgs, "chi")
	if hk.Hops[0].Duplicate != 2 || ty.Peers[0].Duplicate != 2 {
		t.Errorf("hk>ty should keep the route default: %+v %+v", hk.Hops[0], ty.Peers[0])
	}
	if ty.Hops[0].Duplicate != 1 || chi.Peers[0].Duplicate != 1 {
		t.Errorf("ty>chi should carry one copy each way: %+v %+v", ty.Hops[0], chi.Peers[0])
	}
}

// Naming a shared leg settles what its paths could not agree on, and leaves the
// legs they do not share at each path's own count.
func TestLegSettlesSharedLeg(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 1, Transport: "udp", Exit: "chi", Target: hypixel(),
		Paths: []Path{
			{Via: []string{"ty"}, Duplicate: 2},
			{Via: []string{"ty", "sg"}, Duplicate: 3},
		},
		Legs: []Leg{{From: "hk", To: "ty", Duplicate: 2}}})
	cfgs, _ := expandOK(t, top)
	ty := only1(t, cfgs, "ty")
	dup := map[string]int{}
	for _, h := range ty.Hops {
		dup[h.Addr] = h.Duplicate
	}
	if dup["198.51.100.30:9002"] != 2 || dup["198.51.100.40:9001"] != 3 {
		t.Errorf("ty's legs onward: %v", dup)
	}
}

func TestLegMustBeOnAPath(t *testing.T) {
	for _, legs := range [][]Leg{
		{{From: "hk", To: "chi", Duplicate: 1}},
		{{From: "chi", To: "ty", Duplicate: 1}},
		{{From: "ty", To: "chi", Duplicate: 1}, {From: "ty", To: "chi", Duplicate: 2}},
	} {
		top := topo(Route{Name: "r", Entry: "hk", Port: 1, Transport: "udp",
			Via: []string{"ty", "chi"}, Target: hypixel(), Legs: legs})
		if err := top.Routes[0].normalize(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := expand(top); err == nil {
			t.Errorf("accepted legs %+v", legs)
		}
	}
}

func TestLoopRejected(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 1, Transport: "udp", Exit: "chi", Target: hypixel(),
		Paths: []Path{
			{Via: []string{"ty", "sg"}},
			{Via: []string{"sg", "ty"}},
		}})
	if err := top.Routes[0].normalize(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := expand(top); err == nil {
		t.Fatal("accepted paths that loop")
	}
}

func TestNormalizeRejectsImpossibleRoutes(t *testing.T) {
	cases := []struct {
		name string
		r    Route
	}{
		{"duplicate without udp", Route{Name: "r", Duplicate: 2, Via: []string{"chi"}}},
		{"racing without udp", Route{Name: "r", Exit: "chi", Paths: []Path{{}, {}}}},
		{"paths without an exit", Route{Name: "r", Transport: "udp", Paths: []Path{{Via: []string{"ty"}}}}},
		{"via and paths together", Route{Name: "r", Via: []string{"chi"}, Exit: "chi", Paths: []Path{{}}}},
		{"no path at all", Route{Name: "r"}},
		{"unknown transport", Route{Name: "r", Transport: "sctp", Via: []string{"chi"}}},
		{"duplicate below one", Route{Name: "r", Transport: "udp", Via: []string{"chi"}, Duplicate: -1}},
		{"leg without udp", Route{Name: "r", Via: []string{"ty", "chi"}, Legs: []Leg{{From: "ty", To: "chi", Duplicate: 1}}}},
		{"leg below one", Route{Name: "r", Transport: "udp", Via: []string{"ty", "chi"}, Legs: []Leg{{From: "ty", To: "chi"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.r.normalize(); err == nil {
				t.Fatal("accepted a route that cannot work")
			}
		})
	}
}

func TestRepeatedNodeRejected(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 1, Transport: "udp",
		Via: []string{"ty", "ty", "chi"}, Target: hypixel()})
	if err := top.Routes[0].normalize(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := expand(top); err == nil {
		t.Fatal("accepted a path through the same node twice")
	}
}

// Redeploying must not roll the keys: every node would have to be updated at the
// same instant or the chain would break in the middle.
func TestKeysAreStableAcrossExpands(t *testing.T) {
	top := topo(Route{Name: "hypixel", Entry: "hk", Port: 25565, Transport: "udp",
		Via: []string{"ty", "chi"}, Target: hypixel()})
	first, _ := expandOK(t, top)
	second, _, err := expand(top)
	if err != nil {
		t.Fatal(err)
	}
	a := first["hk"].Listeners[0].Hops[0].Key
	b := second["hk"].Listeners[0].Hops[0].Key
	if a != b {
		t.Fatalf("key changed between expands: %s -> %s", a, b)
	}
}

// A single UDP leg with no relay in between is a legitimate topology.
func TestExpandUDPDirect(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Transport: "udp",
		Via: []string{"chi"}, Target: hypixel()})
	cfgs, _ := expandOK(t, top)
	hk := only1(t, cfgs, "hk")
	chi := only1(t, cfgs, "chi")
	if len(hk.Hops) != 1 || hk.Hops[0].Addr != "198.51.100.30:9000" {
		t.Fatalf("hk: %+v", hk.Hops)
	}
	if len(chi.Peers) != 1 || chi.Peers[0].Duplicate != defaultDuplicate {
		t.Fatalf("chi: %+v", chi.Peers)
	}
}

// An entry with a whitelist gets a control link on the port after the hops, with
// a key of its own that survives re-expansion; nothing else does.
func TestControlLinkOnWhitelistedEntries(t *testing.T) {
	top := topo(
		Route{Name: "hypixel", Entry: "hk", Port: 25565, Transport: "udp",
			Via: []string{"ty", "chi"}, Target: hypixel(), Whitelist: "whitelist.txt"},
		Route{Name: "hypixel-ty", Entry: "ty", Port: 25565, Transport: "udp",
			Via: []string{"chi"}, Target: hypixel(), Whitelist: "whitelist.txt"},
	)
	cfgs, _ := expandOK(t, top)

	hk, ty, chi := cfgs["hk"].Control, cfgs["ty"].Control, cfgs["chi"].Control
	if hk == nil || ty == nil {
		t.Fatalf("entries lack a control link: hk=%v ty=%v", hk, ty)
	}
	if chi != nil {
		t.Errorf("the exit has a control link: %+v", chi)
	}
	// Hops took 9000 (ty), 9001 (chi), 9002 (chi again for the second route).
	if hk.Bind != ":9003" || ty.Bind != ":9004" {
		t.Errorf("control ports: hk=%s ty=%s", hk.Bind, ty.Bind)
	}
	if hk.Key == "" || hk.Key == ty.Key {
		t.Errorf("control keys: hk=%q ty=%q", hk.Key, ty.Key)
	}
	if len(hk.AllowFrom) != 0 {
		t.Errorf("without a bot only loopback may connect, got %v", hk.AllowFrom)
	}

	again, _ := expandOK(t, top)
	if again["hk"].Control.Key != hk.Key {
		t.Error("re-expanding minted a new control key")
	}
}

func TestNoControlLinkWithoutWhitelist(t *testing.T) {
	top := topo(Route{Name: "hypixel", Entry: "hk", Port: 25565,
		Via: []string{"ty", "chi"}, Target: hypixel()})
	cfgs, _ := expandOK(t, top)
	for n, c := range cfgs {
		if c.Control != nil {
			t.Errorf("%s has a control link with no whitelist to manage", n)
		}
	}
}
