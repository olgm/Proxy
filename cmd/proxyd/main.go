// Command proxyd is the node runtime. Every node in a chain runs this same binary;
// what it does is entirely determined by its config file.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"

	"github.com/olgm/proxy/internal/proxy"
)

func main() {
	path := flag.String("c", "/etc/proxyd/config.json", "config file")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	b, err := os.ReadFile(*path)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	var cfg proxy.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Fatalf("parse %s: %v", *path, err)
	}
	if err := proxy.Run(&cfg); err != nil {
		log.Fatalf("%v", err)
	}
}
