// Command proxybot is the Discord bot. It runs on one node and reaches every
// entry's control link, so a member's whitelist entry lands on the whole chain.
// Members manage their own accounts within what their roles allow; managers
// manage anyone's. See agents/control-plane.md.
package main

import (
	"flag"
	"log"
	"os"
	"time"

	"github.com/olgm/proxy/internal/botcfg"
)

func main() {
	path := flag.String("c", "/etc/proxyd/bot.json", "bot config")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := botcfg.Load(*path)
	if err != nil {
		log.Fatalf("%v", err)
	}
	// The token comes through the environment, from a root-only file the unit
	// names, so it is never on the command line or in a file this process can
	// read back.
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_BOT_TOKEN is not set")
	}
	ch, err := newChain(cfg)
	if err != nil {
		log.Fatalf("%v", err)
	}
	b := &bot{cfg: cfg, chain: ch, adds: newLimiter(5, time.Minute)}
	b.roster = newRoster(ch, cfg.OnlineNodes, statePath)
	if err := run(b, token); err != nil {
		log.Fatalf("%v", err)
	}
}
