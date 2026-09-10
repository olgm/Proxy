package main

import (
	"strings"
	"testing"

	"github.com/olgm/proxy/internal/botcfg"
	"github.com/olgm/proxy/internal/probe"
)

func feedTopo(f *Feeds) *Topology {
	top := topo(Route{Name: "hypixel", Entry: "hk", Port: 25565, Transport: "udp",
		Via: []string{"ty", "chi"}, Whitelist: "whitelist.txt", Target: hypixel()})
	top.Feeds = f
	top.Probe = &Probe{Hz: 1, Windows: []string{"1m", "10m"}}
	top.Discord = &Discord{Guild: "g", Roles: map[string]botcfg.Role{"r": {Accounts: 1}}}
	return top
}

// A feed that names no variable has nowhere to post. That is a typo, and the
// way to turn a feed off is to delete the block.
func TestFeedsNeedAWebhookVariable(t *testing.T) {
	top := feedTopo(&Feeds{Sessions: &Feed{}})
	if err := top.checkFeeds(); err == nil {
		t.Fatal("accepted a feed with no webhook_env")
	}
}

// A feed nobody is deployed to post is a typo too, and saying so here beats
// deploying a chain that is quietly missing half of what was asked for.
func TestFeedsNeedSomethingToPostThem(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Topology)
	}{
		{"probe without probed", func(top *Topology) {
			top.Feeds = &Feeds{Probe: &ProbeFeed{Feed: Feed{WebhookEnv: "W"}}}
			top.Probe = nil
		}},
		{"online without a bot", func(top *Topology) {
			top.Feeds = &Feeds{Online: &OnlineFeed{Feed: Feed{WebhookEnv: "W"}}}
			top.Discord = nil
		}},
		{"status without a bot", func(top *Topology) {
			top.Feeds = &Feeds{Status: &StatusFeed{Feed: Feed{WebhookEnv: "W"}}}
			top.Discord = nil
		}},
		{"a window probed does not keep", func(top *Topology) {
			top.Feeds = &Feeds{Probe: &ProbeFeed{Feed: Feed{WebhookEnv: "W"}, Windows: []string{"5m"}}}
		}},
		{"a node that does not exist", func(top *Topology) {
			top.Feeds = &Feeds{Online: &OnlineFeed{Feed: Feed{WebhookEnv: "W"}, Nodes: []string{"nowhere"}}}
		}},
		{"a relay, which no player can be online at", func(top *Topology) {
			top.Feeds = &Feeds{Online: &OnlineFeed{Feed: Feed{WebhookEnv: "W"}, Nodes: []string{"ty"}}}
		}},
	} {
		top := feedTopo(nil)
		tc.set(top)
		if err := top.checkFeeds(); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// Shape is checked when the topology loads; the environment is deploy's
// question. Every other command has to keep working in a shell where .env was
// never sourced.
func TestFeedShapeCheckDoesNotReadTheEnvironment(t *testing.T) {
	top := feedTopo(&Feeds{Sessions: &Feed{WebhookEnv: "DEFINITELY_NOT_SET_ANYWHERE"}})
	if err := top.checkFeeds(); err != nil {
		t.Fatalf("shape check read the environment: %v", err)
	}
}

func TestFeedEnvRendersAndRefuses(t *testing.T) {
	t.Setenv("A_HOOK", "https://discord.com/api/webhooks/1/abc")

	body, err := feedEnv(map[string]*Feed{envSessionsWebhook: {WebhookEnv: "A_HOOK"}})
	if err != nil {
		t.Fatalf("feedEnv: %v", err)
	}
	if body != envSessionsWebhook+"=https://discord.com/api/webhooks/1/abc\n" {
		t.Fatalf("body = %q", body)
	}
	// A variable that is not set stops the deploy. The alternative is a chain
	// that looks configured and posts nothing.
	if _, err := feedEnv(map[string]*Feed{envSessionsWebhook: {WebhookEnv: "NOT_SET_AT_ALL"}}); err == nil {
		t.Error("accepted an unset variable")
	}
	t.Setenv("BAD_HOOK", "discord.com/api/webhooks/1/abc")
	if _, err := feedEnv(map[string]*Feed{envSessionsWebhook: {WebhookEnv: "BAD_HOOK"}}); err == nil {
		t.Error("accepted something that is not a URL")
	}
}

// Deleting a feed from the topology has to turn it off, which means removing
// the file rather than leaving the last URL on the node.
func TestEnvFileRemovesWhenThereIsNoFeed(t *testing.T) {
	if got := envFile("/etc/proxyd/feeds.env", "x", ""); got != "$SUDO rm -f /etc/proxyd/feeds.env" {
		t.Fatalf("empty feed does not remove the file: %q", got)
	}
	got := envFile("/etc/proxyd/feeds.env", `"$USER"`, "K=v\n")
	if !strings.Contains(got, "install -m 0640 -o root -g \"$USER\" /dev/stdin /etc/proxyd/feeds.env <<'FEEDENV'\nK=v\nFEEDENV") {
		t.Fatalf("feed file not written root-owned:\n%s", got)
	}
	// The remove has to come first, and has to be there: uutils install cannot
	// overwrite from /dev/stdin, so without it ty deploys once and never again.
	if !strings.HasPrefix(got, "$SUDO rm -f /etc/proxyd/feeds.env\n$SUDO install ") {
		t.Fatalf("feed file not removed before it is written:\n%s", got)
	}
}

// The URL travels inside the script over ssh stdin, never on a command line and
// never through /tmp, which is the path the bot token already takes.
func TestInstallScriptsCarryTheFeedInTheirBody(t *testing.T) {
	const url = "https://discord.com/api/webhooks/1/s3cr3t"
	for _, tc := range []struct {
		name, script, unit string
	}{
		{"proxyd", installScript("proxyd", true, false, false, envSessionsWebhook+"="+url+"\n"),
			"EnvironmentFile=-/etc/proxyd/feeds.env"},
		{"probed", installProbeScript(true, envProbeWebhook+"="+url+"\n"),
			"EnvironmentFile=-/etc/probed/feeds.env"},
		{"proxybot", installBotScript(true, "tok", envOnlineWebhook+"="+url+"\n"),
			"EnvironmentFile=-/etc/proxyd/bot-feeds.env"},
	} {
		if !strings.Contains(tc.script, url) {
			t.Errorf("%s: script does not carry the URL", tc.name)
		}
		if !strings.Contains(tc.script, tc.unit) {
			t.Errorf("%s: unit does not read the feed file:\n%s", tc.name, tc.script)
		}
	}
	// A node with no feed gets the file removed, and the unit still tolerates
	// its absence — that is what the leading - is for.
	off := installScript("proxyd", true, false, false, "")
	if !strings.Contains(off, "rm -f /etc/proxyd/feeds.env") {
		t.Errorf("no feed does not clear the file:\n%s", off)
	}
}

// The session feed is posted by whoever parses a handshake. A relay never sees
// a login and has nothing to say.
func TestSessionFeedNodesAreTheIngresses(t *testing.T) {
	top := topo(
		Route{Name: "a", Entry: "hk", Port: 25565, Via: []string{"ty", "chi"}, Target: hypixel()},
		Route{Name: "b", Entry: "sg", Port: 25566, Via: []string{"chi"}, Target: hypixel()},
	)
	cfgs, _ := expandOK(t, top)
	got := strings.Join(sessionFeedNodes(cfgs), " ")
	if got != "hk sg" {
		t.Fatalf("session feed nodes = %q, want \"hk sg\"", got)
	}
}

// Chicago answers every class and originates none, so it never writes a line
// and is not given a URL it would never use.
func TestProbeFeedSkipsTheNodeThatOnlyAnswers(t *testing.T) {
	cfgs := map[string]*probe.Config{
		"hk":  {Classes: []probe.Class{{Down: []probe.Hop{{Link: 0}}}}},
		"chi": {Classes: []probe.Class{{Up: []probe.Hop{{Link: 0}}}}},
	}
	got := strings.Join(probeOriginators(cfgs), " ")
	if got != "hk" {
		t.Fatalf("originators = %q, want \"hk\"", got)
	}
}

// Named nodes first, in the order given; anything else falls to the end rather
// than off, so an entry added later still shows up in the roster.
func TestOnlineOrderPutsNamedNodesFirst(t *testing.T) {
	top := topo(
		Route{Name: "a", Entry: "hk", Port: 25565, Whitelist: "whitelist.txt", Via: []string{"chi"}, Target: hypixel()},
		Route{Name: "b", Entry: "ty", Port: 25566, Whitelist: "whitelist.txt", Via: []string{"chi"}, Target: hypixel()},
		Route{Name: "c", Entry: "sg", Port: 25567, Whitelist: "whitelist.txt", Via: []string{"chi"}, Target: hypixel()},
	)
	top.Feeds = &Feeds{Online: &OnlineFeed{Feed: Feed{WebhookEnv: "W"}, Nodes: []string{"sg", "hk"}}}
	if got := strings.Join(top.onlineOrder(), " "); got != "sg hk ty" {
		t.Fatalf("order = %q, want \"sg hk ty\"", got)
	}
}

// Without a filter the classes one node originates would post several messages
// a minute. The default is the longest window: the only one whose p99 has
// enough samples behind it to mean anything.
func TestProbeFeedDefaultsToTheLongestWindow(t *testing.T) {
	top := feedTopo(&Feeds{Probe: &ProbeFeed{Feed: Feed{WebhookEnv: "W"}}})
	if got := strings.Join(top.probeFeedWindows(), " "); got != "10m" {
		t.Fatalf("windows = %q, want \"10m\"", got)
	}
	top.Feeds.Probe.Windows = []string{"1m"}
	if got := strings.Join(top.probeFeedWindows(), " "); got != "1m" {
		t.Fatalf("windows = %q, want \"1m\"", got)
	}
}
