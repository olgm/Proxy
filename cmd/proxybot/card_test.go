package main

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// samples are the two states worth looking at: everything up, and a fault of
// each kind at once.
func samples() map[string]cardData {
	at := time.Date(2026, 9, 10, 10, 53, 0, 0, time.UTC)
	up := cardData{
		version: "v2.2.2 (c640893)", at: at,
		nodes: []cardNode{
			{node: "au", proxyd: true, probed: true, probedKnown: true, leg: "au→ch", ms: 176.5, playersKnown: true},
			{node: "hk", proxyd: true, probed: true, probedKnown: true, leg: "hk→ch", ms: 220.4, playersKnown: true},
			{node: "ty", proxyd: true, probed: true, probedKnown: true, leg: "ty→ch", ms: 121.5, playersKnown: true},
			{node: "ch", proxyd: true, probed: true, probedKnown: true, playersKnown: true},
		},
		nodesUp: 4, nodesTotal: 4, classesUp: 9, classesTotal: 9,
	}
	bad := cardData{
		version: "v2.2.2 (c640893)", at: at,
		nodes: []cardNode{
			{node: "au", proxyd: true, probed: true, probedKnown: true, leg: "au→ch", ms: 176.5, players: 1, playersKnown: true},
			{node: "hk", proxyd: true, probed: true, probedKnown: true, leg: "hk→ch", ms: 220.4, players: 2, playersKnown: true},
			{node: "ty", health: healthDegraded, proxyd: true, probedKnown: true, leg: "ty→ch", ms: -1, playersKnown: true},
			{node: "ch", health: healthDown, downFor: 3 * time.Minute},
		},
		nodesUp: 3, nodesTotal: 4, classesUp: 3, classesTotal: 9, online: 3,
	}
	return map[string]cardData{"up": up, "down": bad}
}

// TestRenderCard checks the card is a PNG of the size the layout says, and
// writes the samples out when CARD_OUT names a directory — the only way to
// judge a drawing is to look at it, and this is how it gets looked at.
func TestRenderCard(t *testing.T) {
	for name, d := range samples() {
		b, err := renderCard(d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: not a png: %v", name, err)
		}
		if got, want := img.Bounds().Dx(), px(pagePad*2+cardW); got != want {
			t.Errorf("%s: width %d, want %d", name, got, want)
		}
		if dy := img.Bounds().Dy(); dy < 400 || dy > 700 {
			t.Errorf("%s: height %d is not a card", name, dy)
		}
		if dir := os.Getenv("CARD_OUT"); dir != "" {
			if err := os.WriteFile(filepath.Join(dir, name+".png"), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A node with no health link has no probed column at all, rather than one
// reading "down": nothing was deployed there to ask.
func TestCardOmitsUndeployedProbed(t *testing.T) {
	f := face(szRow, false)
	with := width(services(cardNode{proxyd: true, probed: true, probedKnown: true}, f))
	without := width(services(cardNode{proxyd: true}, f))
	if without >= with {
		t.Fatalf("a row with no health link is %d wide, one with is %d", without, with)
	}
}

func TestBrief(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "<1m"},
		{3 * time.Minute, "3m"},
		{90 * time.Minute, "1h30m"},
	} {
		if got := brief(c.d); got != c.want {
			t.Errorf("brief(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
