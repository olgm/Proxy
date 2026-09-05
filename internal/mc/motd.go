package mc

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// defaultMotd is what an ingress says about itself when no file is configured.
// Carried over from the v1 proxy, favicon and sample included, so the listing a
// player already has saved does not change under them.
//
// version.name is never rendered: the client only shows it when the protocol does
// not match, and Render always echoes the client's own. The branding that is
// always visible is the description.
const defaultMotd = `{
  "version": {"name": ".w. v2", "protocol": 47},
  "players": {
    "max": 1,
    "online": 1,
    "sample": [{"name": ".w.", "id": "ee0c5510-68a3-44dd-970e-64ad5fcf1e78"}]
  },
  "description": {"text": ".w. v2"},
  "favicon": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAIAAAAlC+aJAAAAAXNSR0IArs4c6QAAAANzQklUCAgI2+FP4AAAAbpJREFUaIHt1aGr8lAYBvDDYTBEZEEUEYNM1hRMBrEZlhSTDIcsiAhiFEwGyzAMu1gMBptoWdBgFHXBMCaL4t8xbhBE9HqvH5xP+T6eX3zf95zzHMZhhAAAAAAAAAAAAMD/TxCETqdDKWW1IZuNEomELMuvxMpms7Ise57H5FzC6gKNRmM4HMbj8V8n0+n0crlkcugFmwssFgvP85LJ5G0xGo1WKhW/339bTKVS+/2eyaEsUUqn06lhGNeKJEm2bZ/P536/fztpWVY4HH57wBfkcjnLsjiOI4RwHGea5mQyUVX1dDoVi8XLjCAIx+PxozGfo5SappnP5wkh5XLZsqxQKEQIqdVqtm1LkkQIyWQy8/n8w0F/oCjKaDSilK5WK03TrnVd19frdTAY1DRtMBh8MOEvfD7fZrOp1+vb7Zbn+Wud5/nZbDYejw3DaLVad6tEUSyVSu9N+ly73XYc5+7hEkJEUbRt23XdQqFw12o2m47jMPy1vURV1Vgs9liPRCKHw0HX9ceWoiiu614ew53bz/UOPM/vdrtqtfptt9vt9nq9Zwv/Zq4/EQgEPh0BAAAAAAAAAADgX/YFcR18foaYG5YAAAAASUVORK5CYII="
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
