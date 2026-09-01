// Package mojang reads Mojang's public profile API, the only place the binding
// between a Minecraft name and a UUID actually lives.
//
// Nothing on our wire carries that binding: a client before 1.19 sends a bare name,
// and names are recycled once released, so a name recorded months ago may belong to
// someone else today. Asking here is what makes a name-matched login meaningful.
// These calls go to Mojang, never to Hypixel, so the probe rule in
// agents/operational-safety.md does not apply to them.
package mojang

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultProfileURL = "https://sessionserver.mojang.com/session/minecraft/profile/"
	defaultNameURL    = "https://api.mojang.com/users/profiles/minecraft/"
	timeout           = 5 * time.Second
	maxBody           = 1 << 16
)

type Client struct {
	http       *http.Client
	gate       *Gate
	profileURL string
	nameURL    string
}

func New() *Client {
	return &Client{
		http:       &http.Client{Timeout: timeout},
		gate:       NewGate(),
		profileURL: defaultProfileURL,
		nameURL:    defaultNameURL,
	}
}

type profile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// NameFor returns the name a UUID currently answers to. This is the refresh side:
// a controlled daily walk of a list we chose, so it is not rate limited here.
func (c *Client) NameFor(uuid string) (string, bool) {
	p, ok := c.get(c.profileURL + strings.ReplaceAll(uuid, "-", ""))
	if !ok {
		return "", false
	}
	return p.Name, p.Name != ""
}

// UUIDFor returns the UUID that owns name right now, when the limiters allow a
// lookup. This is the login side, reached only by a login that already failed the
// local check, so every call is attacker-triggered and every call is gated.
func (c *Client) UUIDFor(name, ip string) (string, bool) {
	if !c.gate.Allow(name, ip) {
		return "", false
	}
	// The name is whatever the client typed, not necessarily a legal one. Escape it:
	// unescaped it would be free rein over the request path.
	p, ok := c.get(c.nameURL + url.PathEscape(name))
	if !ok {
		return "", false
	}
	return p.ID, p.ID != ""
}

func (c *Client) get(url string) (*profile, bool) {
	resp, err := c.http.Get(url)
	if err != nil {
		log.Printf("mojang: %v", err)
		return nil, false
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent, http.StatusNotFound:
		return nil, false // no such name, or no such profile
	default:
		log.Printf("mojang: %s: %s", url, resp.Status)
		return nil, false
	}

	var p profile
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&p); err != nil {
		log.Printf("mojang: %s: %v", url, err)
		return nil, false
	}
	return &p, true
}
