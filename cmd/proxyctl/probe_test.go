package main

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/olgm/proxy/internal/probe"
)

func probeOK(t *testing.T, top *Topology) map[string]*probe.Config {
	t.Helper()
	top.Probe = &Probe{Hz: 1, Windows: []string{"1m", "10m"}}
	expandOK(t, top)
	cfgs, _, _, err := expandProbe(top, top.nextPort)
	if err != nil {
		t.Fatalf("expandProbe: %v", err)
	}
	return cfgs
}

// classes describes one node's job as "name/kind xN role", sorted, which is the
// whole of what a node was told to measure.
func classes(t *testing.T, cfgs map[string]*probe.Config, node string) []string {
	t.Helper()
	c, ok := cfgs[node]
	if !ok {
		t.Fatalf("no probe config for %q", node)
	}
	var out []string
	for _, k := range c.Classes {
		role := "relay"
		switch {
		case len(k.Up) == 0:
			role = "origin"
		case len(k.Down) == 0:
			role = "answer"
		}
		out = append(out, k.Name+"/"+k.Kind+" x"+itoa(k.Duplicate)+" "+role)
	}
	sort.Strings(out)
	return out
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return strings.TrimSpace(strings.Join([]string{string(rune('0' + n/10)), string(rune('0' + n%10))}, ""))
}

func eq(t *testing.T, got, want []string, what string) {
	t.Helper()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("%s:\n got %v\nwant %v", what, got, want)
	}
}

// A leg is measured twice, at one copy and at the count it carries, and the two
// classes differ only in that count. Without both there is nothing to subtract.
func TestProbeMeasuresEachLegAtBothCounts(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Transport: "udp",
		Duplicate: 2, Via: []string{"ty"}, Target: backend()})
	cfgs := probeOK(t, top)

	eq(t, classes(t, cfgs, "hk"), []string{"hk>ty/leg x1 origin", "hk>ty/leg x2 origin"}, "hk")
	eq(t, classes(t, cfgs, "ty"), []string{"hk>ty/leg x1 answer", "hk>ty/leg x2 answer"}, "ty")
	if _, ok := cfgs["sg"]; ok {
		t.Error("sg is on no route and must not be probed")
	}
}

// A leg that already carries one copy is measured once: the baseline and the
// production class would be the same measurement under two names.
func TestProbeDoesNotMeasureAnUnduplicatedLegTwice(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Transport: "udp",
		Duplicate: 1, Via: []string{"ty"}, Target: backend()})
	cfgs := probeOK(t, top)
	eq(t, classes(t, cfgs, "hk"), []string{"hk>ty/leg x1 origin"}, "hk")
}

// The pairs probed are the pairs production uses. Nothing polls a path players
// never travel, which is the rule the whole tool is scoped by.
func TestProbeOnlyTouchesProductionPaths(t *testing.T) {
	top := topo(
		Route{Name: "hk", Entry: "hk", Port: 25565, Transport: "udp", Duplicate: 2,
			Via: []string{"ty", "chi"}, Target: backend()},
		Route{Name: "sg", Entry: "sg", Port: 25565, Transport: "udp", Duplicate: 2,
			Via: []string{"chi"}, Target: backend()})
	cfgs := probeOK(t, top)

	seen := map[string]bool{}
	for _, c := range cfgs {
		for _, k := range c.Classes {
			if k.Kind == probe.KindLeg {
				seen[k.Name] = true
			}
		}
	}
	want := []string{"hk>ty", "sg>chi", "ty>chi"}
	var got []string
	for k := range seen {
		got = append(got, k)
	}
	sort.Strings(got)
	eq(t, got, want, "legs probed")

	// sg and hk are both ingresses and never talk to each other, so that pair is
	// not a leg and must never be polled.
	if seen["hk>sg"] || seen["sg>hk"] || seen["sg>ty"] {
		t.Error("probed a pair no route puts traffic between")
	}
}

// A chain is only worth its own class when it is more than one leg. sg>chi is a
// single leg and is already measured as one.
func TestProbeChainsOnlyMultiLegRoutes(t *testing.T) {
	top := topo(
		Route{Name: "hk", Entry: "hk", Port: 25565, Transport: "udp", Duplicate: 2,
			Via: []string{"ty", "chi"}, Target: backend()},
		Route{Name: "sg", Entry: "sg", Port: 25565, Transport: "udp", Duplicate: 2,
			Via: []string{"chi"}, Target: backend()})
	cfgs := probeOK(t, top)

	eq(t, classes(t, cfgs, "hk"), []string{
		"hk>chi/chain x1 origin", "hk>chi/chain x2 origin",
		"hk>ty/leg x1 origin", "hk>ty/leg x2 origin"}, "hk")
	eq(t, classes(t, cfgs, "sg"), []string{"sg>chi/leg x1 origin", "sg>chi/leg x2 origin"}, "sg")
	eq(t, classes(t, cfgs, "ty"), []string{
		"hk>chi/chain x1 relay", "hk>chi/chain x2 relay",
		"hk>ty/leg x1 answer", "hk>ty/leg x2 answer",
		"ty>chi/leg x1 origin", "ty>chi/leg x2 origin"}, "ty")
	eq(t, classes(t, cfgs, "chi"), []string{
		"hk>chi/chain x1 answer", "hk>chi/chain x2 answer",
		"sg>chi/leg x1 answer", "sg>chi/leg x2 answer",
		"ty>chi/leg x1 answer", "ty>chi/leg x2 answer"}, "chi")
}

// A chain whose legs all carry one copy is measured once, for the same reason a
// leg at one copy is: the baseline and the production class would be the same
// measurement, written to the dataset under the same name and the same number.
func TestProbeDoesNotMeasureAnUnduplicatedChainTwice(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Transport: "udp",
		Duplicate: 1, Via: []string{"ty", "chi"}, Target: backend()})
	cfgs := probeOK(t, top)

	eq(t, classes(t, cfgs, "hk"), []string{"hk>chi/chain x1 origin", "hk>ty/leg x1 origin"}, "hk")
	eq(t, classes(t, cfgs, "ty"), []string{
		"hk>chi/chain x1 relay", "hk>ty/leg x1 answer", "ty>chi/leg x1 origin"}, "ty")
	eq(t, classes(t, cfgs, "chi"), []string{
		"hk>chi/chain x1 answer", "ty>chi/leg x1 answer"}, "chi")
}

// A chain carries each leg's own count, not one number for the whole path, so a
// route with a quieter leg is probed the way it really runs.
func TestProbeChainCarriesPerLegCounts(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Transport: "udp", Duplicate: 3,
		Via: []string{"ty", "chi"}, Legs: []Leg{{From: "ty", To: "chi", Duplicate: 1}},
		Target: backend()})
	cfgs := probeOK(t, top)

	var got []int
	for _, k := range cfgs["ty"].Classes {
		if k.Kind == probe.KindChain && k.Duplicate == 3 {
			for _, h := range k.Down {
				got = append(got, h.Duplicate)
			}
		}
	}
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("ty>chi under the production chain should carry 1 copy, got %v", got)
	}
	// And the leg class for that same edge agrees, because it is the same leg.
	eq(t, classes(t, cfgs, "chi"), []string{
		"hk>chi/chain x1 answer", "hk>chi/chain x3 answer", "ty>chi/leg x1 answer"}, "chi")
}

// Racing gives a node more than one way to the exit, and every one of them has to
// be probed: the point of the class is that the first answer back wins.
func TestProbeRacesEveryPath(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Transport: "udp", Duplicate: 2,
		Exit: "chi", Paths: []Path{{Via: []string{"ty"}}, {Via: nil}}, Target: backend()})
	cfgs := probeOK(t, top)

	for _, k := range cfgs["hk"].Classes {
		if k.Kind != probe.KindChain {
			continue
		}
		if len(k.Down) != 2 {
			t.Errorf("chain class %q has %d ways to the exit, want 2", k.Name, len(k.Down))
		}
	}
	// The direct leg is production too, so it is measured on its own as well.
	eq(t, classes(t, cfgs, "hk"), []string{
		"hk>chi/chain x1 origin", "hk>chi/chain x2 origin",
		"hk>chi/leg x1 origin", "hk>chi/leg x2 origin",
		"hk>ty/leg x1 origin", "hk>ty/leg x2 origin"}, "hk")
}

// Only a node something is probed toward needs a port and a firewall rule. An
// ingress dials out and answers come back on the same socket.
func TestProbePortsAndChecks(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Transport: "udp", Duplicate: 2,
		Via: []string{"ty"}, Target: backend()})
	top.Probe = &Probe{Hz: 1}
	expandOK(t, top)
	cfgs, checks, _, err := expandProbe(top, top.nextPort)
	if err != nil {
		t.Fatal(err)
	}
	if cfgs["hk"].Bind != "" {
		t.Errorf("hk originates only and should bind nothing, got %q", cfgs["hk"].Bind)
	}
	if cfgs["ty"].Bind == "" {
		t.Error("ty is probed toward and needs a bind address")
	}
	if len(checks) != 1 {
		t.Fatalf("%d checks, want 1", len(checks))
	}
	c := checks[0]
	if c.from != "hk" || c.to != "ty" || !c.udp || c.service() != "probed" {
		t.Errorf("wrong check: %+v", c)
	}
	if !strings.Contains(ufwRule(top, c), "proto udp") {
		t.Errorf("probe rule should open udp: %s", ufwRule(top, c))
	}
}

// The probe ports come after the hops and the control links, so adding the block
// never moves a port proxyd is already using.
func TestProbePortsFollowTheRest(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Transport: "udp", Duplicate: 2,
		Via: []string{"ty"}, Whitelist: "whitelist.txt", Target: backend()})
	top.Probe = &Probe{Hz: 1}
	cfgs, _ := expandOK(t, top)
	pcfgs, _, _, err := expandProbe(top, top.nextPort)
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, c := range cfgs {
		for _, l := range c.Listeners {
			used[l.Bind] = true
		}
		if c.Control != nil {
			used[c.Control.Bind] = true
		}
	}
	for n, c := range pcfgs {
		if c.Bind != "" && used[c.Bind] {
			t.Errorf("%s: probe bound to %s, which proxyd already uses", n, c.Bind)
		}
	}
}

// Two routes crossing the same leg must agree about it: it is one leg, one key
// and one number of copies whatever the routes think.
func TestProbeRejectsRoutesDisagreeingAboutALeg(t *testing.T) {
	top := topo(
		Route{Name: "a", Entry: "hk", Port: 25565, Transport: "udp", Duplicate: 2,
			Via: []string{"ty", "chi"}, Target: backend()},
		Route{Name: "b", Entry: "sg", Port: 25566, Transport: "udp", Duplicate: 3,
			Via: []string{"ty", "chi"}, Target: backend()})
	top.Probe = &Probe{Hz: 1}
	expandOK(t, top)
	if _, _, _, err := expandProbe(top, top.nextPort); err == nil {
		t.Fatal("accepted two routes asking for different counts on ty>chi")
	} else if !strings.Contains(err.Error(), "ty>chi") {
		t.Errorf("error should name the leg: %v", err)
	}
}

// A topology with no UDP leg has nothing to measure, and saying so beats deploying
// a service that would sit there silent.
func TestProbeRejectsATopologyWithNoUDPLeg(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Via: []string{"ty"}, Target: backend()})
	top.Probe = &Probe{Hz: 1}
	expandOK(t, top)
	if _, _, _, err := expandProbe(top, top.nextPort); err == nil {
		t.Fatal("accepted a topology with no udp leg")
	}
}

// Every class the nodes of one leg hold has to agree on its id, or they are
// talking about different measurements with the same byte on the wire.
func TestProbeClassIDsAgreeAcrossNodes(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Transport: "udp", Duplicate: 2,
		Via: []string{"ty", "chi"}, Target: backend()})
	cfgs := probeOK(t, top)

	byID := map[uint8]string{}
	for _, n := range probeNodes(cfgs) {
		for _, k := range cfgs[n].Classes {
			label := k.Name + "/" + k.Kind + "/" + itoa(k.Duplicate)
			if old, ok := byID[k.ID]; ok && old != label {
				t.Errorf("class id %d is %q on one node and %q on another", k.ID, old, label)
			}
			byID[k.ID] = label
		}
	}
	// And every class a node holds names a link it actually has.
	for _, n := range probeNodes(cfgs) {
		c := cfgs[n]
		for _, k := range c.Classes {
			for _, h := range append(append([]probe.Hop{}, k.Up...), k.Down...) {
				if h.Link < 0 || h.Link >= len(c.Links) {
					t.Fatalf("%s: class %q names link %d of %d", n, k.Name, h.Link, len(c.Links))
				}
			}
		}
	}
}

// Both ends of a leg must hold the same key, or nothing opens.
func TestProbeKeysMatchAcrossALeg(t *testing.T) {
	top := topo(Route{Name: "r", Entry: "hk", Port: 25565, Transport: "udp", Duplicate: 2,
		Via: []string{"ty"}, Target: backend()})
	cfgs := probeOK(t, top)

	if cfgs["hk"].Links[0].Key != cfgs["ty"].Links[0].Key {
		t.Error("the two ends of hk>ty hold different keys")
	}
	if cfgs["hk"].Links[0].Key == "" {
		t.Fatal("no key minted for hk>ty")
	}
	// The probe key is its own: probed holding it must not be a way into a session.
	for _, c := range cfgs {
		for _, l := range c.Links {
			if l.Key == top.keys.get("r", "hk", "ty") {
				t.Error("probed was given the tunnel's key for a leg")
			}
		}
	}
}

// The generated configs have to be ones probed will actually accept.
func TestProbeConfigsAreValid(t *testing.T) {
	top := topo(
		Route{Name: "hk", Entry: "hk", Port: 25565, Transport: "udp", Duplicate: 2,
			Via: []string{"ty", "chi"}, Target: backend()},
		Route{Name: "sg", Entry: "sg", Port: 25566, Transport: "udp", Duplicate: 2,
			Via: []string{"chi"}, Target: backend()})
	cfgs := probeOK(t, top)
	for _, n := range probeNodes(cfgs) {
		c := *cfgs[n]
		c.Log = filepath.Join(t.TempDir(), "probe.jsonl")
		if c.Bind != "" {
			c.Bind = "127.0.0.1:0"
		}
		for i := range c.Links {
			// The addresses name real nodes; only the shape is under test here.
			if strings.Contains(c.Links[i].Addr, ":") {
				c.Links[i].Addr = "127.0.0.1:65000"
			} else {
				c.Links[i].Addr = "127.0.0.1"
			}
		}
		node, err := probe.New(c)
		if err != nil {
			t.Fatalf("%s: probed would reject its own config: %v", n, err)
		}
		node.Close()
	}
}
