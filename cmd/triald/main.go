// Command triald measures candidate legs against the ones a route already runs
// on, so a node can be chosen on evidence.
//
// It is a sibling of probed and not a replacement for it. probed watches the
// production path and may never be pointed off it; triald exists precisely to
// measure paths no route uses yet, which is why its legs are configured by hand
// rather than derived from the routes. Both can run on the same node: separate
// binary, unit, user, port and keys, and neither knows the other is there.
//
// It never reaches the backend. Every leg here ends on a machine we run. See
// agents/operational-safety.md.
//
// It is temporary. When the trial has answered its question, delete it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/olgm/proxy/internal/trial"
)

func main() {
	path := flag.String("c", "/etc/triald/config.json", "config file")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := readConfig(*path)
	if err != nil {
		log.Fatalf("%v", err)
	}
	n, err := trial.New(*cfg)
	if err != nil {
		log.Fatalf("%v", err)
	}
	n.Start()
	log.Printf("%s: trial on %d legs at %.3g Hz, %s", cfg.Name, len(cfg.Legs), cfg.Hz, cfg.Log)

	// Close on a signal rather than exiting under it, so the last window in flight
	// is written instead of lost on a restart.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	n.Close()
}

func readConfig(path string) (*trial.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg trial.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}
