package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// trialRec is every record kind in the trial dataset flattened, because one file
// holds all three and each section below cares about a different handful.
type trialRec struct {
	K    string `json:"k"`
	T    string `json:"t"`
	Leg  string `json:"leg"`
	Tick uint64 `json:"tick"`
	Path string `json:"path"`
	W    string `json:"w"`

	N    int     `json:"n"`
	Sent int     `json:"sent"`
	Got  int     `json:"got"`
	Loss float64 `json:"loss"`
	P50  float64 `json:"p50"`
	P99  float64 `json:"p99"`
	Mdev float64 `json:"mdev"`

	Expect float64 `json:"expect"`
	Err    string  `json:"err"`

	// Class is probed's name for what trial calls Leg. Reading both into one
	// struct is what lets the incumbent and the candidate be printed in one table.
	Class string `json:"class"`
}

func (r trialRec) label() string {
	if r.Leg != "" {
		return r.Leg
	}
	return r.Class
}

// trialReport reads what pull fetched and answers the two questions the mesh was
// built for. It touches no network and no node.
func trialReport(args []string) error {
	dir := trialDataDir
	if len(args) > 0 {
		dir = args[0]
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no datasets in %s; run `proxyctl trial pull` first", dir)
	}

	byNode := map[string][]trialRec{}
	var probed []trialRec
	for _, f := range files {
		recs, err := readTrialFile(f)
		if err != nil {
			return err
		}
		base := filepath.Base(f)
		if strings.HasSuffix(base, ".probe.jsonl") {
			probed = append(probed, recs...)
			continue
		}
		byNode[strings.TrimSuffix(base, ".trial.jsonl")] = recs
	}

	nodes := make([]string, 0, len(byNode))
	for n := range byNode {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	reportLegs(byNode, probed)
	reportRacing(nodes, byNode)
	reportCorrelation(nodes, byNode)
	reportTraces(nodes, byNode)
	return nil
}

func readTrialFile(path string) ([]trialRec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []trialRec
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		var r trialRec
		// A line truncated by a crash mid-write is skipped rather than failing the
		// read: the records before it are still good.
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// reportLegs is the replacement question: how does a candidate leg compare with
// the one it would replace. probed's rows are printed in the same table because
// they were computed by the same code — internal/window — which is the only
// reason putting them side by side means anything.
func reportLegs(byNode map[string][]trialRec, probed []trialRec) {
	type agg struct {
		windows, sent, got int
		p50s, p99s, mdevs  []float64
		expect             float64
	}
	collect := func(recs []trialRec, longest string) map[string]*agg {
		out := map[string]*agg{}
		for _, r := range recs {
			if r.K != "" && r.K != "win" {
				continue
			}
			if r.W != longest || r.label() == "" {
				continue
			}
			a := out[r.label()]
			if a == nil {
				a = &agg{}
				out[r.label()] = a
			}
			a.windows++
			a.sent += r.Sent
			a.got += r.Got
			a.expect = r.Expect
			if r.N > 0 {
				a.p50s = append(a.p50s, r.P50)
				a.p99s = append(a.p99s, r.P99)
				a.mdevs = append(a.mdevs, r.Mdev)
			}
		}
		return out
	}

	var all []trialRec
	for _, recs := range byNode {
		all = append(all, recs...)
	}
	legs := collect(all, longestWindow(all))
	inc := collect(probed, longestWindow(probed))

	fmt.Println("== legs")
	fmt.Println("Round trips. mdev is the population standard deviation, as ping(8) reports it.")
	fmt.Printf("\n%-24s %7s %8s %8s %8s %8s %8s %9s\n",
		"leg", "windows", "p50", "p99", "mdev", "loss%", "expect", "vs expect")
	for _, name := range sortedAggs(legs) {
		a := legs[name]
		printLeg(name, a.windows, a.sent, a.got, a.p50s, a.p99s, a.mdevs, a.expect)
	}
	if len(inc) > 0 {
		fmt.Printf("\n%-24s %7s %8s %8s %8s %8s\n", "incumbent (probed)", "windows", "p50", "p99", "mdev", "loss%")
		for _, name := range sortedAggs(inc) {
			a := inc[name]
			printLeg(name, a.windows, a.sent, a.got, a.p50s, a.p99s, a.mdevs, 0)
		}
	}
	fmt.Println()
}

func printLeg(name string, windows, sent, got int, p50s, p99s, mdevs []float64, expect float64) {
	loss := 0.0
	if sent > 0 {
		loss = 100 * float64(sent-got) / float64(sent)
	}
	if len(p50s) == 0 {
		fmt.Printf("%-24s %7d %8s %8s %8s %8.2f\n", name, windows, "-", "-", "-", loss)
		return
	}
	p50, p99, mdev := median(p50s), worst(p99s), median(mdevs)
	if expect <= 0 {
		fmt.Printf("%-24s %7d %8.2f %8.2f %8.2f %8.2f\n", name, windows, p50, p99, mdev, loss)
		return
	}
	fmt.Printf("%-24s %7d %8.2f %8.2f %8.2f %8.2f %8.1f %+9.2f\n",
		name, windows, p50, p99, mdev, loss, expect, p50-expect)
}

// reportRacing is the first half of the second question: when copies of one probe
// arrive over different routes, which route got there first and by how much.
//
// Both arrival times are read off the same node's clock, which is what makes the
// margin a real number. Nothing here compares a timestamp taken on one machine
// with one taken on another; that difference would be a clock offset.
func reportRacing(nodes []string, byNode map[string][]trialRec) {
	fmt.Println("== racing")
	fmt.Println("Which route delivered a tick first, compared on the receiving node's own clock.")
	any := false
	for _, node := range nodes {
		byTick := arrivalsByTick(byNode[node])
		wins := map[string]int{}
		margins := map[string][]float64{}
		races := 0
		for _, as := range byTick {
			if len(as) < 2 {
				continue
			}
			races++
			sort.Slice(as, func(i, j int) bool { return as[i].T < as[j].T })
			wins[as[0].Path]++
			if d, ok := gapMS(as[0].T, as[1].T); ok {
				margins[as[0].Path] = append(margins[as[0].Path], d)
			}
		}
		if races == 0 {
			continue
		}
		any = true
		fmt.Printf("\n%s: %d ticks arrived by more than one route\n", node, races)
		fmt.Printf("    %-34s %8s %8s %14s\n", "route", "wins", "win%", "median margin")
		for _, p := range sortedCounts(wins) {
			m := "-"
			if len(margins[p]) > 0 {
				m = fmt.Sprintf("%.3f ms", median(margins[p]))
			}
			fmt.Printf("    %-34s %8d %7.1f%% %14s\n", p, wins[p], 100*float64(wins[p])/float64(races), m)
		}
	}
	if !any {
		fmt.Println("\nNo node saw one tick arrive by two routes. Either the mesh is not flooding,")
		fmt.Println("or nothing has been collected yet.")
	}
	fmt.Println()
}

// reportCorrelation is the half that actually decides whether a second node earns
// its keep. Two routes that are a millisecond apart on a calm day are worth
// nothing as a race if they fail at the same instants.
//
// A tick nobody delivered leaves no record at all, so the tick space is taken as
// the contiguous range between the first and last seen. That is what makes a total
// outage visible rather than invisible.
func reportCorrelation(nodes []string, byNode map[string][]trialRec) {
	fmt.Println("== independence")
	fmt.Println("Do two routes lose the same ticks? phi is 0 when they fail independently and")
	fmt.Println("1 when they always fail together. A racing pair that fails together is one")
	fmt.Println("node's cost for no benefit.")

	for _, node := range nodes {
		byTick := arrivalsByTick(byNode[node])
		if len(byTick) == 0 {
			continue
		}
		lo, hi := ^uint64(0), uint64(0)
		paths := map[string]bool{}
		for tick, as := range byTick {
			lo, hi = min(lo, tick), max(hi, tick)
			for _, a := range as {
				paths[a.Path] = true
			}
		}
		if len(paths) < 2 {
			continue
		}
		names := make([]string, 0, len(paths))
		for p := range paths {
			names = append(names, p)
		}
		sort.Strings(names)

		total := int(hi-lo) + 1
		delivered := map[string]map[uint64]bool{}
		for _, p := range names {
			delivered[p] = map[uint64]bool{}
		}
		for tick, as := range byTick {
			for _, a := range as {
				delivered[a.Path][tick] = true
			}
		}

		fmt.Printf("\n%s: %d ticks between first and last arrival\n", node, total)
		fmt.Printf("    %-34s %10s %10s\n", "route", "delivered", "lost%")
		lossy := false
		for _, p := range names {
			lost := total - len(delivered[p])
			if lost > 0 {
				lossy = true
			}
			fmt.Printf("    %-34s %10d %9.3f%%\n", p, len(delivered[p]), 100*float64(lost)/float64(total))
		}
		if !lossy {
			fmt.Println("    Every route delivered every tick. Independence cannot be assessed from")
			fmt.Println("    a window with no failures in it - this is not evidence that they are")
			fmt.Println("    independent, only that nothing has gone wrong yet.")
			continue
		}
		fmt.Printf("\n    %-24s %-24s %8s %12s %12s\n", "route", "against", "phi", "both lost", "if independent")
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				a, b := names[i], names[j]
				var both, onlyA, onlyB, neither float64
				for t := lo; t <= hi; t++ {
					da, db := delivered[a][t], delivered[b][t]
					switch {
					case da && db:
						both++
					case da:
						onlyA++
					case db:
						onlyB++
					default:
						neither++
					}
				}
				n := float64(total)
				pBoth := neither / n
				pInd := ((onlyA + neither) / n) * ((onlyB + neither) / n)
				fmt.Printf("    %-24s %-24s %8s %11.4f%% %11.4f%%\n",
					shorten(a, 24), shorten(b, 24), phiString(both, onlyA, onlyB, neither),
					100*pBoth, 100*pInd)
			}
		}
	}
	fmt.Println()
}

func reportTraces(nodes []string, byNode map[string][]trialRec) {
	n := 0
	for _, node := range nodes {
		for _, r := range byNode[node] {
			if r.K == "trace" {
				n++
			}
		}
	}
	if n == 0 {
		fmt.Println("== traces\n\nNo leg went far enough past its expected round trip to be traced.")
		fmt.Println("If that is a surprise, check expect_ms in trial.json against reality.")
		fmt.Println()
		return
	}
	fmt.Printf("== traces\n\n%d paths were walked. Each carries mtr's own json; grep them out with:\n", n)
	fmt.Printf("    jq -c 'select(.k==\"trace\")' %s/*.trial.jsonl\n\n", trialDataDir)
	fmt.Printf("%-12s %-24s %10s %10s\n", "node", "leg", "p50", "expect")
	for _, node := range nodes {
		for _, r := range byNode[node] {
			if r.K != "trace" {
				continue
			}
			note := ""
			if r.Err != "" {
				note = "  " + r.Err
			}
			fmt.Printf("%-12s %-24s %10.2f %10.1f%s\n", node, r.Leg, r.P50, r.Expect, note)
		}
	}
	fmt.Println()
}

func arrivalsByTick(recs []trialRec) map[uint64][]trialRec {
	out := map[uint64][]trialRec{}
	for _, r := range recs {
		if r.K == "rx" {
			out[r.Tick] = append(out[r.Tick], r)
		}
	}
	return out
}

// gapMS is the distance between two RFC3339Nano stamps, in milliseconds. Both come
// off one machine, so this is a duration and not a difference of clocks.
func gapMS(a, b string) (float64, bool) {
	ta, err1 := parseNano(a)
	tb, err2 := parseNano(b)
	if !err1 || !err2 {
		return 0, false
	}
	return float64(tb-ta) / 1e6, true
}

func longestWindow(recs []trialRec) string {
	best, bestN := "", -1
	for _, r := range recs {
		if r.K != "" && r.K != "win" {
			continue
		}
		if n := windowSeconds(r.W); n > bestN {
			best, bestN = r.W, n
		}
	}
	return best
}

func windowSeconds(w string) int {
	if w == "" {
		return -1
	}
	n := 0
	for i := 0; i < len(w)-1; i++ {
		if w[i] < '0' || w[i] > '9' {
			return -1
		}
		n = n*10 + int(w[i]-'0')
	}
	switch w[len(w)-1] {
	case 's':
		return n
	case 'm':
		return n * 60
	case 'h':
		return n * 3600
	}
	return -1
}

func phiString(both, onlyA, onlyB, neither float64) string {
	den := math.Sqrt((both + onlyA) * (onlyB + neither) * (both + onlyB) * (onlyA + neither))
	if den == 0 {
		return "-"
	}
	return fmt.Sprintf("%.3f", (both*neither-onlyA*onlyB)/den)
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func worst(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)-1]
}

func sortedAggs[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCounts(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n+1:]
}

func parseNano(s string) (int64, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, false
	}
	return t.UnixNano(), true
}
