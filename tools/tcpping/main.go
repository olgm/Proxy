// Command tcpping measures round-trip time with TCP instead of ICMP.
//
// A node can rate-limit or deprioritise ICMP without touching TCP, so an ICMP
// number is not the number our traffic actually sees. This dials, times the
// SYN -> SYN-ACK exchange, and resets the connection. That is one round trip on
// the same protocol proxyd carries.
//
// Point it at a peer's tailscale address to time the tunnel, or at a public
// address to time the raw path. Same tool, the destination picks the route.
//
// The target needs something accepting on that port; -listen provides one.
//
// Not built or installed by proxyctl. Build and copy it by hand:
//
//	go build -o tcpping ./tools/tcpping
package main

import (
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"sort"
	"time"
)

func main() {
	var (
		count    = flag.Int("c", 20, "probes to send")
		interval = flag.Duration("i", 200*time.Millisecond, "wait between probes")
		timeout  = flag.Duration("w", 2*time.Second, "per-probe timeout")
		listen   = flag.String("listen", "", "accept and close on this address instead of probing")
		four     = flag.Bool("4", false, "IPv4 only")
		six      = flag.Bool("6", false, "IPv6 only")
		quiet    = flag.Bool("q", false, "summary only")
	)
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: tcpping [flags] <host:port>")
		fmt.Fprintln(os.Stderr, "       tcpping -listen <[host]:port>")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *listen != "" {
		serve(*listen)
		return
	}
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if *four && *six {
		fmt.Fprintln(os.Stderr, "tcpping: -4 and -6 are mutually exclusive")
		os.Exit(2)
	}
	network := "tcp"
	if *four {
		network = "tcp4"
	} else if *six {
		network = "tcp6"
	}
	os.Exit(probe(network, flag.Arg(0), *count, *interval, *timeout, *quiet))
}

// serve accepts connections and drops them immediately. It exists so a node can
// be a ping target on a port of our choosing without running anything else.
func serve(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tcpping: listen:", err)
		os.Exit(1)
	}
	fmt.Println("tcpping: listening on", ln.Addr())
	for {
		c, err := ln.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "tcpping: accept:", err)
			return
		}
		c.Close()
	}
}

func probe(network, target string, count int, interval, timeout time.Duration, quiet bool) int {
	fmt.Printf("tcpping %s (%s)\n", target, network)
	var rtts []float64
	for i := 0; i < count; i++ {
		if i > 0 {
			time.Sleep(interval)
		}
		start := time.Now()
		c, err := net.DialTimeout(network, target, timeout)
		// connect(2) returns once the SYN-ACK lands, so this is one round trip.
		// The final ACK goes out without us waiting for it.
		rtt := time.Since(start)
		if err != nil {
			fmt.Printf("seq=%d %v\n", i, err)
			continue
		}
		if t, ok := c.(*net.TCPConn); ok {
			t.SetLinger(0) // reset rather than FIN, so we leave no TIME_WAIT behind
		}
		c.Close()
		rtts = append(rtts, float64(rtt.Nanoseconds())/1e6)
		if !quiet {
			fmt.Printf("seq=%d time=%.3f ms\n", i, rtts[len(rtts)-1])
		}
	}
	return summarise(target, count, rtts)
}

func summarise(target string, sent int, rtts []float64) int {
	fmt.Printf("\n--- %s tcpping statistics ---\n", target)
	loss := float64(sent-len(rtts)) / float64(sent) * 100
	fmt.Printf("%d probes, %d ok, %.0f%% loss\n", sent, len(rtts), loss)
	if len(rtts) == 0 {
		return 1
	}
	var sum, sumsq float64
	for _, v := range rtts {
		sum += v
		sumsq += v * v
	}
	n := float64(len(rtts))
	mean := sum / n
	// mdev as ping reports it: population standard deviation.
	mdev := math.Sqrt(math.Max(0, sumsq/n-mean*mean))
	sorted := append([]float64(nil), rtts...)
	sort.Float64s(sorted)
	fmt.Printf("rtt min/avg/max/mdev = %.3f/%.3f/%.3f/%.3f ms\n",
		sorted[0], mean, sorted[len(sorted)-1], mdev)
	fmt.Printf("rtt p50/p90 = %.3f/%.3f ms\n", pct(sorted, 50), pct(sorted, 90))
	return 0
}

func pct(sorted []float64, p int) float64 {
	i := p * (len(sorted) - 1) / 100
	return sorted[i]
}
