package main

// The status card is drawn here, pixel by pixel, rather than described to
// Discord as an embed. An embed is laid out by whatever client is reading it:
// the columns that make this readable at a glance — every node's services and
// its leg on one line — collapse on a narrow phone into a paragraph nobody
// scans. An image is the same shape everywhere.
//
// The font is Go Mono, which x/image ships as bytes. That is the whole reason
// it is Go Mono and not the Menlo the mockup was drawn in: a card rendered on a
// node must not depend on a font being installed there, and a bundled .ttf is a
// binary blob in the repo that nothing else would ever read. The card is
// monospace throughout, so a column is an advance width and never a measurement.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// scale draws everything at twice the geometry below. Discord scales an image
// down to fit the column it is shown in and never up, so a card drawn at 1x
// arrives soft on every display that has more pixels than points — which is all
// of the phones this is read on.
const scale = 2

// The geometry, in the units the mockup was designed in. Every one of these is
// a CSS pixel from that mockup and is multiplied by scale on the way to a
// coordinate, so the two can still be compared side by side.
const (
	pagePad  = 14 // the margin Discord's own background shows through
	cardW    = 760
	cardPadX = 26
	cardPadT = 22
	cardPadB = 20
	cardRad  = 10

	headGap   = 18
	rowH      = 32
	ruleAbove = 14
	ruleBelow = 13
	ruleH     = 1

	colDot  = 22 // the status dot's cell, not the dot
	colNode = 52
	colSvc  = 214
	colLeg  = 200
	dotD    = 9

	pillPadX = 11
	pillPadY = 4
	pillRad  = 6
	footGap  = 12
	sepPad   = 7
)

// Type sizes, also from the mockup's stylesheet.
const (
	szTitle = 17
	szStamp = 13
	szRow   = 14.5
	szNode  = 15.5
	szFoot  = 15
	szPill  = 14
)

// The palette is Discord's own dark theme, so the card sits on the channel
// background rather than on top of it.
var (
	colPage   = hex(0x1e1f22)
	colCard   = hex(0x2b2d31)
	colRule   = hex(0x3f4147)
	colBright = hex(0xf2f3f5) // a heading, or a figure worth reading
	colText   = hex(0xdbdee1) // an ordinary value
	colDim    = hex(0xb5bac1)
	colLabel  = hex(0x949ba4) // the word in front of a value
	colFaint  = hex(0x80848e)
	colSep    = hex(0x4e5058)
	colZero   = hex(0x5c6069) // a value that is nothing, and reads as nothing

	colUp   = hex(0x23a55a)
	colDown = hex(0xf23f43)
	colWarn = hex(0xf0b232)

	colPillOKBg  = hex(0x1a4d33)
	colPillOKFg  = hex(0x3ecf74)
	colPillBadBg = hex(0x59201f)
	colPillBadFg = hex(0xff6b6e)
)

func hex(v uint32) color.RGBA {
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

// px converts a mockup coordinate to a device one.
func px(v float64) int { return int(math.Round(v * scale)) }

// health is how a node is doing, which is one of three things and not two. A
// node that answers with a service down is a different fault from a node that
// does not answer at all: the first can still be asked what is wrong.
type health int

const (
	healthUp health = iota
	healthDegraded
	healthDown
)

// cardNode is one row.
type cardNode struct {
	node   string
	health health
	// proxyd and probed are only read when health is not healthDown: a node
	// that did not answer has no service state to report, and saying "proxyd
	// down" about a machine that is off the network claims more than was seen.
	proxyd, probed bool
	// probedKnown is false for a node with no health link deployed. It has no
	// probed column rather than an empty one.
	probedKnown bool
	// downFor is how long the node has been unreachable, for the row that
	// replaces the service columns.
	downFor time.Duration

	// leg is the class this node's row reports, already rendered ("au→ch"), and
	// ms its latest p50. ms is negative when no window has closed yet, which is
	// what a probed that just restarted looks like. An empty leg is a node that
	// originates nothing: the exit.
	leg string
	ms  float64

	// players is how many sessions the node is relaying, and playersKnown is
	// false when it could not be asked. Nobody online and could-not-ask are
	// different facts and the card does not merge them.
	players      int
	playersKnown bool
}

// cardData is everything the card shows.
type cardData struct {
	version string
	at      time.Time
	nodes   []cardNode

	// nodesUp and classesUp are the footer's two fractions. Classes are counted
	// rather than derived from the rows because a node that is up with probed
	// down still contributes its classes to the denominator: they are
	// configured, they are simply not being measured right now.
	nodesUp, nodesTotal     int
	classesUp, classesTotal int
	online                  int
}

// renderCard draws the card and encodes it. It is pure: everything it needs is
// in cardData, which is what lets a test render one and look at it.
func renderCard(d cardData) ([]byte, error) {
	title, stamp := face(szTitle, true), face(szStamp, false)
	row, node := face(szRow, false), face(szNode, true)
	foot, pill := face(szFoot, false), face(szPill, true)

	headH := height(title)
	footH := px(pillPadY*2) + height(pill)
	cardH := px(cardPadT) + headH + px(headGap) + len(d.nodes)*px(rowH) +
		px(ruleAbove) + px(ruleH) + px(ruleBelow) + footH + px(cardPadB)

	w := px(pagePad*2 + cardW)
	img := image.NewRGBA(image.Rect(0, 0, w, px(pagePad)*2+cardH))
	draw.Draw(img, img.Bounds(), image.NewUniform(colPage), image.Point{}, draw.Src)

	card := image.Rect(px(pagePad), px(pagePad), w-px(pagePad), px(pagePad)+cardH)
	roundRect(img, card, px(cardRad), colCard)

	x0 := card.Min.X + px(cardPadX)
	x1 := card.Max.X - px(cardPadX)
	y := card.Min.Y + px(cardPadT)

	// The heading: what this is and when it was drawn. The version is
	// version.String() verbatim, so a card is traceable to the binary that drew
	// it — which is the only reason a build revision is on a status board.
	base := y + ascent(title)
	drawRuns(img, x0, base, []span{
		{"probed ", colBright, title},
		{d.version, colFaint, title},
	})
	right(img, x1, y+ascent(title), []span{{d.at.UTC().Format("15:04 utc"), colFaint, stamp}})
	y += headH + px(headGap)

	for _, n := range d.nodes {
		mid := y + (px(rowH)+ascent(row)-descent(row))/2
		disc(img, x0+px(dotD)/2, y+px(rowH)/2, px(dotD)/2, dotColour(n.health))
		drawRuns(img, x0+px(colDot), y+(px(rowH)+ascent(node)-descent(node))/2,
			[]span{{n.node, colBright, node}})

		svc := x0 + px(colDot+colNode)
		if n.health == healthDown {
			// The service columns are replaced rather than filled in with
			// guesses: an unreachable node was not asked about its services.
			drawRuns(img, svc, mid, []span{
				{"unreachable", colDown, row},
				{" for " + brief(n.downFor), colLabel, row},
			})
		} else {
			drawRuns(img, svc, mid, services(n, row))
			drawRuns(img, x0+px(colDot+colNode+colSvc), mid, legRuns(n, row))
		}
		right(img, x1, mid, playerSpans(n, row))
		y += px(rowH)
	}

	y += px(ruleAbove)
	draw.Draw(img, image.Rect(x0, y, x1, y+px(ruleH)), image.NewUniform(colRule), image.Point{}, draw.Src)
	y += px(ruleH) + px(ruleBelow)

	// The footer is the line this whole card exists for: the one-glance answer.
	down := d.nodesTotal - d.nodesUp
	label, bg, fg := "all nodes active", colPillOKBg, colPillOKFg
	if down > 0 {
		label, bg, fg = fmt.Sprintf("%d node%s down", down, plural(down)), colPillBadBg, colPillBadFg
	}
	pw := width([]span{{label, fg, pill}}) + px(pillPadX)*2
	roundRect(img, image.Rect(x0, y, x0+pw, y+footH), px(pillRad), bg)
	drawRuns(img, x0+px(pillPadX), y+px(pillPadY)+ascent(pill), []span{{label, fg, pill}})

	sep := span{"·", colSep, foot}
	pad := span{spaces(px(sepPad), foot), colSep, foot}
	drawRuns(img, x0+pw+px(footGap), y+(footH+ascent(foot)-descent(foot))/2, []span{
		{fmt.Sprintf("%d/%d", d.nodesUp, d.nodesTotal), colBright, foot},
		{" nodes", colDim, foot}, pad, sep, pad,
		{fmt.Sprintf("%d/%d", d.classesUp, d.classesTotal), colBright, foot},
		{" probe classes", colDim, foot}, pad, sep, pad,
		{fmt.Sprintf("%d", d.online), colBright, foot},
		{" online", colDim, foot},
	})

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func dotColour(h health) color.RGBA {
	switch h {
	case healthDegraded:
		return colWarn
	case healthDown:
		return colDown
	}
	return colUp
}

// services renders the two service words. probed is left out entirely on a node
// with no health link rather than shown as unknown: nothing was deployed to ask.
func services(n cardNode, f font.Face) []span {
	rs := []span{{"proxyd ", colLabel, f}, word(n.proxyd, f)}
	if n.probedKnown {
		rs = append(rs, span{"   probed ", colLabel, f}, word(n.probed, f))
	}
	return rs
}

func word(up bool, f font.Face) span {
	if up {
		return span{"up", colUp, f}
	}
	return span{"down", colDown, f}
}

// legRuns renders the node's own measurement. A latency that has not been
// reported yet is an em dash and not a zero, which would read as instant.
func legRuns(n cardNode, f font.Face) []span {
	if n.leg == "" {
		return []span{{"exit", colLabel, f}}
	}
	if n.ms < 0 {
		return []span{{n.leg + " ", colLabel, f}, {"—", colText, f}}
	}
	return []span{
		{n.leg + " ", colLabel, f},
		{fmt.Sprintf("%.1f", n.ms), colText, f},
		{" ms", colLabel, f},
	}
}

func playerSpans(n cardNode, f font.Face) []span {
	switch {
	case !n.playersKnown:
		return []span{{"—", colZero, f}}
	case n.players == 0:
		return []span{{"no players", colZero, f}}
	}
	return []span{{fmt.Sprintf("%d player%s", n.players, plural(n.players)), colText, f}}
}

// brief is how long something has been wrong, at the resolution anyone reads it
// at. Under a minute is "less than a minute" and not "0m": a fault that has just
// started is news, and a zero reads like a rounding error.
func brief(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// spaces is a run of blanks that measures at least w device pixels, which is how
// CSS padding is expressed in a monospace line.
func spaces(w int, f font.Face) string {
	adv, _ := f.GlyphAdvance(' ')
	n := 1
	if a := adv.Ceil(); a > 0 {
		n = (w + a - 1) / a
	}
	return fmt.Sprintf("%*s", n, "")
}

// span is a stretch of text in one colour and one face.
type span struct {
	text string
	col  color.RGBA
	face font.Face
}

func drawRuns(dst draw.Image, x, baseline int, rs []span) {
	d := font.Drawer{Dst: dst, Dot: fixed.P(x, baseline)}
	for _, r := range rs {
		d.Src, d.Face = image.NewUniform(r.col), r.face
		d.DrawString(r.text)
	}
}

// right draws runs ending at x, which is what a column of counts wants.
func right(dst draw.Image, x, baseline int, rs []span) {
	drawRuns(dst, x-width(rs), baseline, rs)
}

func width(rs []span) int {
	var w fixed.Int26_6
	for _, r := range rs {
		w += font.MeasureString(r.face, r.text)
	}
	return w.Ceil()
}

func ascent(f font.Face) int  { return f.Metrics().Ascent.Ceil() }
func descent(f font.Face) int { return f.Metrics().Descent.Ceil() }
func height(f font.Face) int  { return ascent(f) + descent(f) }

// roundRect fills a rectangle with rounded corners. The corners are covered
// analytically rather than supersampled: the distance from the corner's centre
// gives a coverage within half a pixel, which at this radius is a smooth edge
// and costs one square root per corner pixel.
func roundRect(dst draw.Image, r image.Rectangle, rad int, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			blend(dst, x, y, c, cornerCoverage(r, rad, x, y))
		}
	}
}

func cornerCoverage(r image.Rectangle, rad int, x, y int) float64 {
	cx, cy := 0, 0
	switch {
	case x < r.Min.X+rad:
		cx = r.Min.X + rad
	case x >= r.Max.X-rad:
		cx = r.Max.X - rad - 1
	default:
		return 1
	}
	switch {
	case y < r.Min.Y+rad:
		cy = r.Min.Y + rad
	case y >= r.Max.Y-rad:
		cy = r.Max.Y - rad - 1
	default:
		return 1
	}
	return coverage(float64(x)-float64(cx), float64(y)-float64(cy), float64(rad))
}

// disc fills a circle, the status dot.
func disc(dst draw.Image, cx, cy, rad int, c color.RGBA) {
	for y := cy - rad - 1; y <= cy+rad+1; y++ {
		for x := cx - rad - 1; x <= cx+rad+1; x++ {
			blend(dst, x, y, c, coverage(float64(x-cx)+0.5, float64(y-cy)+0.5, float64(rad)))
		}
	}
}

// coverage is how much of the pixel at (dx, dy) from a circle's centre is
// inside a circle of that radius, to within half a pixel.
func coverage(dx, dy, rad float64) float64 {
	d := math.Hypot(dx, dy)
	switch {
	case d <= rad-0.5:
		return 1
	case d >= rad+0.5:
		return 0
	}
	return rad + 0.5 - d
}

func blend(dst draw.Image, x, y int, c color.RGBA, a float64) {
	if a <= 0 {
		return
	}
	if a >= 1 {
		dst.Set(x, y, c)
		return
	}
	r0, g0, b0, _ := dst.At(x, y).RGBA()
	mix := func(src uint8, dst uint32) uint8 {
		return uint8(float64(src)*a + float64(dst>>8)*(1-a))
	}
	dst.Set(x, y, color.RGBA{mix(c.R, r0), mix(c.G, g0), mix(c.B, b0), 0xff})
}

// Faces are built once and shared. opentype.NewFace is not cheap and the card is
// redrawn every minute for as long as the bot runs.
var (
	faceMu    sync.Mutex
	faceCache = map[[2]int]font.Face{}
	monoReg   = mustParse(gomono.TTF)
	monoBold  = mustParse(gomonobold.TTF)
)

func mustParse(b []byte) *opentype.Font {
	f, err := opentype.Parse(b)
	if err != nil {
		panic("card: " + err.Error()) // a font compiled into the binary cannot fail at runtime
	}
	return f
}

// face returns the face for a mockup type size. The size is scaled here, so
// every call site names the size the stylesheet did.
func face(size float64, bold bool) font.Face {
	key := [2]int{int(size * scale * 64), b2i(bold)}
	faceMu.Lock()
	defer faceMu.Unlock()
	if f, ok := faceCache[key]; ok {
		return f
	}
	src := monoReg
	if bold {
		src = monoBold
	}
	f, err := opentype.NewFace(src, &opentype.FaceOptions{
		Size: size * scale, DPI: 72, Hinting: font.HintingFull,
	})
	if err != nil {
		panic("card: " + err.Error())
	}
	faceCache[key] = f
	return f
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
