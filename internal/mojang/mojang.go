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
	"errors"
	"fmt"
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

// ErrNoSuchPlayer is Mojang's answer for a name nobody holds or a UUID that is no
// profile. Any other error means the question went unanswered, which a caller may
// say so about, or retry; it must never be read as "no".
var ErrNoSuchPlayer = errors.New("mojang: no such player")

// NameFor returns the name a UUID currently answers to. This is the refresh side:
// a controlled daily walk of a list we chose, so it is not rate limited here.
func (c *Client) NameFor(uuid string) (string, bool) {
	name, err := c.LookupUUID(uuid)
	return name, err == nil
}

// UUIDFor returns the UUID that owns name right now, when the limiters allow a
// lookup. This is the login side, reached only by a login that already failed the
// local check, so every call is attacker-triggered and every call is gated.
func (c *Client) UUIDFor(name, ip string) (string, bool) {
	if !c.gate.Allow(name, ip) {
		return "", false
	}
	uuid, _, err := c.LookupName(name)
	return uuid, err == nil
}

// LookupName returns the UUID that owns name right now, and the name's canonical
// spelling. Not gated: it is for the control link, which only a key holder can
// reach, never for anything a connecting client can trigger.
func (c *Client) LookupName(name string) (uuid, canonical string, err error) {
	// The name is whatever was typed, not necessarily a legal one. Escape it:
	// unescaped it would be free rein over the request path.
	p, err := c.get(c.nameURL + url.PathEscape(name))
	if err != nil {
		return "", "", err
	}
	return p.ID, p.Name, nil
}

// LookupUUID returns the name a UUID answers to right now. Not gated, as
// LookupName.
func (c *Client) LookupUUID(uuid string) (string, error) {
	p, err := c.get(c.profileURL + strings.ReplaceAll(uuid, "-", ""))
	if err != nil {
		return "", err
	}
	return p.Name, nil
}

func (c *Client) get(url string) (*profile, error) {
	resp, err := c.http.Get(url)
	if err != nil {
		log.Printf("mojang: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent, http.StatusNotFound:
		return nil, ErrNoSuchPlayer
	default:
		log.Printf("mojang: %s: %s", url, resp.Status)
		return nil, fmt.Errorf("mojang: %s", resp.Status)
	}

	var p profile
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&p); err != nil {
		log.Printf("mojang: %s: %v", url, err)
		return nil, fmt.Errorf("mojang: %w", err)
	}
	if p.ID == "" || p.Name == "" {
		return nil, fmt.Errorf("mojang: profile without id or name")
	}
	return &p, nil
}
