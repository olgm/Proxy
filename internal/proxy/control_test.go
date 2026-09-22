package proxy

import (
	"os"
	"strings"
	"testing"

	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/tunnel"
)

const notchBare = "069a79f444e94726a5befca90e38aaf5"

func TestControlLinkManagesTheWhitelist(t *testing.T) {
	useMojang(t, map[string]string{"notch": notchBare})
	p := whitelistFile(t, "# friends\n")
	key := tunnel.NewKey()
	n, err := start(&Config{
		Listeners: []Listener{{Bind: "127.0.0.1:0", Upstream: "127.0.0.1:9",
			Minecraft: &Minecraft{RewriteHost: "mc.example.com", Whitelist: p}}},
		Control: &Control{Bind: "127.0.0.1:0", Key: tunnel.EncodeKey(key)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.close)

	c := &control.Client{Addr: n.ctl.Addr().String(), Key: key}
	rep, err := c.Add("notch", "", "discord:1")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.Entry == nil || rep.Entry.UUID != notchUUID {
		t.Fatalf("Add = %+v", rep)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "# friends\nnotch:"+notchUUID+" # discord:1\n" {
		t.Fatalf("file is %q", got)
	}
	es, err := c.List()
	if err != nil || len(es) != 1 {
		t.Fatalf("List = %+v, %v", es, err)
	}
}

func TestControlNeedsAWhitelist(t *testing.T) {
	_, err := start(&Config{
		Listeners: []Listener{{Bind: "127.0.0.1:0", Upstream: "127.0.0.1:9"}},
		Control:   &Control{Bind: "127.0.0.1:0", Key: tunnel.EncodeKey(tunnel.NewKey())},
	})
	if err == nil || !strings.Contains(err.Error(), "whitelist") {
		t.Fatalf("control without a whitelist: %v", err)
	}
}

func TestControlRejectsBadKey(t *testing.T) {
	p := whitelistFile(t, "")
	_, err := start(&Config{
		Listeners: []Listener{{Bind: "127.0.0.1:0", Upstream: "127.0.0.1:9",
			Minecraft: &Minecraft{RewriteHost: "mc.example.com", Whitelist: p}}},
		Control: &Control{Bind: "127.0.0.1:0", Key: "not-a-key"},
	})
	if err == nil {
		t.Fatal("a control link with a bad key started")
	}
}
