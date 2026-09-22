package mc

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// defaultMotd is what an ingress says about itself when no file is configured.
// It is deliberately neutral, with no favicon and no players sample, since any
// branding belongs in an operator's own /etc/proxyd/motd.json.
//
// version.name is never rendered: the client only shows it when the protocol does
// not match, and Render always echoes the client's own. The branding that is
// always visible is the description.
const defaultMotd = `{
  "version": {"name": "proxy", "protocol": 47},
  "players": {
    "max": 1,
    "online": 1
  },
  "description": {"text": "Minecraft proxy"}
}`

// Motd is the server-list response an ingress answers with. It is loaded once and
// rendered per request, because two fields have to follow the client rather than
// the file: the protocol version, and the live player count.
type Motd struct {
	mu   sync.RWMutex
	base map[string]any
}

// LoadMotd reads a MOTD from path. An empty path gives the built-in one.
func LoadMotd(path string) (*Motd, error) {
	b := []byte(defaultMotd)
	if path != "" {
		var err error
		if b, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("mc: motd: %w", err)
		}
	}
	var base map[string]any
	if err := json.Unmarshal(b, &base); err != nil {
		return nil, fmt.Errorf("mc: motd %s: %w", path, err)
	}
	return &Motd{base: base}, nil
}

// Render builds the status JSON for one client.
//
// version.protocol is echoed from the handshake. Reporting our own would put the
// red "incompatible" badge on the listing for every client that is not exactly
// that version, which is most of them.
func (m *Motd) Render(proto int32, online int) string {
	m.mu.RLock()
	out := make(map[string]any, len(m.base)+2)
	for k, v := range m.base {
		out[k] = v
	}
	version := child(m.base, "version")
	players := child(m.base, "players")
	m.mu.RUnlock()

	version["protocol"] = proto
	players["online"] = online
	if _, ok := players["max"]; !ok {
		players["max"] = online
	}
	out["version"], out["players"] = version, players

	b, err := json.Marshal(out)
	if err != nil {
		// Every value came out of a JSON document, so this cannot fire; an empty
		// response is still better than dropping the connection if it somehow does.
		return "{}"
	}
	return string(b)
}

// child copies one nested object out of the base document, so a render can set
// fields on it without writing through to the shared base.
func child(base map[string]any, key string) map[string]any {
	src, _ := base[key].(map[string]any)
	dst := make(map[string]any, len(src)+2)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
