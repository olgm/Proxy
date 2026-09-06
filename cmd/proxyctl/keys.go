package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/olgm/proxy/internal/tunnel"
)

// keysFile sits beside the topology and is gitignored for the same reason
// topology.json is: it names real infrastructure, and here it also holds the only
// thing standing between a UDP relay and anyone who can spoof a source address.
const keysFile = "tunnel-keys.json"

// keyring holds one key per tunnel leg, so redeploying does not roll every key
// and cut the chain. Keys are minted on demand and written only by deploy —
// config previews without touching anything.
type keyring struct {
	path  string
	keys  map[string]string
	dirty bool
}

func loadKeys(topology string) (*keyring, error) {
	k := &keyring{path: filepath.Join(filepath.Dir(topology), keysFile), keys: map[string]string{}}
	b, err := os.ReadFile(k.path)
	if errors.Is(err, fs.ErrNotExist) {
		return k, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &k.keys); err != nil {
		return nil, fmt.Errorf("parse %s: %w", k.path, err)
	}
	for id, v := range k.keys {
		if _, err := tunnel.DecodeKey(v); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", k.path, id, err)
		}
	}
	return k, nil
}

// get returns the key for one leg. Legs are keyed by the pair of nodes rather
// than by direction, so both ends of a link agree without having to be told.
func (k *keyring) get(route, a, b string) string {
	if a > b {
		a, b = b, a
	}
	id := route + "|" + a + "|" + b
	if v, ok := k.keys[id]; ok {
		return v
	}
	v := tunnel.EncodeKey(tunnel.NewKey())
	k.keys[id] = v
	k.dirty = true
	return v
}

// probe returns the key sealing one leg of the measurement service. It is not the
// leg's tunnel key on purpose: probed runs as its own user and holds only these,
// so having them is not a way into a live session.
func (k *keyring) probe(a, b string) string {
	if a > b {
		a, b = b, a
	}
	id := "probe|" + a + "|" + b
	if v, ok := k.keys[id]; ok {
		return v
	}
	v := tunnel.EncodeKey(tunnel.NewKey())
	k.keys[id] = v
	k.dirty = true
	return v
}

// control returns the key sealing one node's control link. The bot's node is
// given every entry's; each entry holds only its own.
// probeHealth is probed's own key for its health link. It is deliberately not
// the node's control key: holding probed's keys must not be a way into a
// session, and that is exactly as true of this one as of the leg keys.
func (k *keyring) probeHealth(node string) string {
	id := "probe-ctl|" + node
	if v, ok := k.keys[id]; ok {
		return v
	}
	v := tunnel.EncodeKey(tunnel.NewKey())
	k.keys[id] = v
	k.dirty = true
	return v
}

func (k *keyring) control(node string) string {
	id := "ctl|" + node
	if v, ok := k.keys[id]; ok {
		return v
	}
	v := tunnel.EncodeKey(tunnel.NewKey())
	k.keys[id] = v
	k.dirty = true
	return v
}

func (k *keyring) save() error {
	if !k.dirty {
		return nil
	}
	b, err := json.MarshalIndent(k.keys, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(k.path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("== keys    wrote %s\n", k.path)
	return nil
}
