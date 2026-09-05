// Command probed measures the legs a route runs on, continuously, and writes the
// numbers to a file.
//
// It is optional and separate from proxyd on purpose: a node can run the chain
// without it, and it can be stopped without touching a live session. Every node on
// a probed route runs this same binary; what it does is entirely determined by its
// config file, the same way proxyd works.
//
// It never reaches the backend. A chain probe stops at the exit exactly as the
// tunnel's own ECHO does. See agents/operational-safety.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/olgm/proxy/internal/probe"
)

func main() {
	path := flag.String("c", "/etc/probed/config.json", "config file")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := readConfig(*path)
	if err != nil {
		log.Fatalf("%v", err)
	}
	n, err := probe.New(*cfg)
	if err != nil {
		log.Fatalf("%v", err)
	}
	n.Start()
	log.Printf("%s: probing %d classes at %.3g Hz, %s", cfg.Name, len(cfg.Classes), cfg.Hz, cfg.Log)

	// Close on a signal rather than exiting under it, so the last window in flight
	// is written instead of lost on a restart.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	n.Close()
}

func readConfig(path string) (*probe.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg probe.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}
