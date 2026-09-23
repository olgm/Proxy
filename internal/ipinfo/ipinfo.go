// Package ipinfo asks ipinfo.io where a player's network is — the place it is
// geolocated to and the AS that holds it — without telling it which address.
//
// The question is always about a prefix: an IPv4 address is asked about as the
// first address of its /24, an IPv6 one as the first of its /48. That is the
// granularity geolocation data is kept at anyway, so the answer is the same one
// the player's own address would get, and the address itself never leaves the
// node. Each prefix is asked about once for as long as proxyd runs.
//
// These calls go to ipinfo.io, never to the backend, so the probe rule in
// agents/operational-safety.md does not apply to them.
package ipinfo

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// DefaultURL is ipinfo.io's lookup endpoint. No token: one question per prefix
// is far inside what it answers without one.
const DefaultURL = "https://ipinfo.io/"

const (
	bits4   = 24
	bits6   = 48
	timeout = 5 * time.Second
	maxBody = 1 << 16
)

// retryAfter is how long a prefix whose lookup failed waits before a login from it
// may ask again. A variable so a test need not wait it out.
var retryAfter = 10 * time.Minute

// cgnat is shared address space (RFC 6598). netip calls it neither private nor
// anything else, and a tailnet address lives in it.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Info is what ipinfo.io said about one prefix.
type Info struct {
	// Net is the prefix that was asked about.
	Net     string `json:"net"`
	City    string `json:"city,omitempty"`
	Region  string `json:"region,omitempty"`
	Country string `json:"country,omitempty"`
	// Org is the AS and its holder, as in "AS4766 Korea Telecom".
	Org string `json:"org,omitempty"`
}

// String is the place and the network in one line, the way an operator reads it:
// "Seoul, KR · AS4766 Korea Telecom". A region that repeats the city is dropped.
func (i Info) String() string {
	var place []string
	for _, s := range []string{i.City, i.Region, i.Country} {
		if s != "" && (len(place) == 0 || place[len(place)-1] != s) {
			place = append(place, s)
		}
	}
	out := strings.Join(place, ", ")
	if i.Org != "" {
		if out != "" {
			out += " · "
		}
		out += i.Org
	}
	if out == "" {
		return "unknown"
	}
	return out
}

// Prefix is the network ip is asked about as, and false for an address not worth
// asking about: unparseable, loopback, private, link-local or shared.
func Prefix(ip string) (netip.Prefix, bool) {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return netip.Prefix{}, false
	}
	a = a.Unmap().WithZone("")
	if !a.IsGlobalUnicast() || a.IsPrivate() || cgnat.Contains(a) {
		return netip.Prefix{}, false
	}
	bits := bits6
	if a.Is4() {
		bits = bits4
	}
	return netip.PrefixFrom(a, bits).Masked(), true
}

// Client remembers what it has been told about every prefix it has seen. A nil
// Client is valid and knows nothing, which is how a node with lookups off runs.
type Client struct {
	url  string
	http *http.Client

	mu   sync.Mutex
	nets map[netip.Prefix]*lookup
}

type lookup struct {
	info   *Info
	busy   bool
	failed time.Time
}

func New(url string) *Client {
	return &Client{
		url:  url,
		http: &http.Client{Timeout: timeout},
		nets: map[netip.Prefix]*lookup{},
	}
}

// Watch starts a lookup of ip's prefix unless it is already known, already being
// asked about, or failed less than retryAfter ago. It returns at once: a login
// never waits on ipinfo.io.
func (c *Client) Watch(ip string) {
	p, ok := Prefix(ip)
	if c == nil || !ok {
		return
	}
	c.mu.Lock()
	l := c.nets[p]
	if l == nil {
		l = &lookup{}
		c.nets[p] = l
	}
	if l.info != nil || l.busy || time.Since(l.failed) < retryAfter {
		c.mu.Unlock()
		return
	}
	l.busy = true
	c.mu.Unlock()

	go func() {
		info, err := c.ask(p)
		c.mu.Lock()
		l.busy = false
		if err != nil {
			l.failed = time.Now()
		} else {
			l.info = &info
		}
		c.mu.Unlock()
		if err != nil {
			log.Printf("ipinfo: %s: %v", p, err)
		} else {
			log.Printf("ipinfo: %s is %s", p, info)
		}
	}()
}

// Get returns what is known about ip's prefix, or nil when nothing is yet.
func (c *Client) Get(ip string) *Info {
	p, ok := Prefix(ip)
	if c == nil || !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.nets[p]; l != nil && l.info != nil {
		info := *l.info
		return &info
	}
	return nil
}

func (c *Client) ask(p netip.Prefix) (Info, error) {
	req, err := http.NewRequest("GET", c.url+p.Addr().String()+"/json", nil)
	if err != nil {
		return Info{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("%s", resp.Status)
	}
	var r struct {
		City    string `json:"city"`
		Region  string `json:"region"`
		Country string `json:"country"`
		Org     string `json:"org"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&r); err != nil {
		return Info{}, fmt.Errorf("decode: %w", err)
	}
	// A bogon comes back with nothing but "bogon": true, which is an answer too
	// and will not change, so it is kept like any other.
	return Info{Net: p.String(), City: r.City, Region: r.Region, Country: r.Country, Org: r.Org}, nil
}
