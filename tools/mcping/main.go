// Command mcping sends a Minecraft server list ping and prints the MOTD, to check
// that a chain is actually carrying traffic to the backend.
//
// It claims a deliberately wrong hostname by default: a backend that answers anyway
// proves the ingress rewrote the address, rather than the client having asked for
// the right thing to begin with.
//
// Not built or installed by proxyctl. Build and copy it by hand:
//
//	go build -o mcping ./tools/mcping
package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/olgm/proxy/internal/mc"
)

const protocol = 765 // 1.20.4

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mcping <host:port> [claimed-hostname]")
		os.Exit(2)
	}
	dial := os.Args[1]
	claim := "not-the-real-hostname.invalid"
	if len(os.Args) > 2 {
		claim = os.Args[2]
	}

	start := time.Now()
	c, err := net.DialTimeout("tcp", dial, 15*time.Second)
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer c.Close()
	connected := time.Since(start)
	c.SetDeadline(time.Now().Add(20 * time.Second))

	hs := (&mc.Handshake{
		ProtocolVersion: protocol,
		Address:         claim,
		Port:            25565,
		Intent:          mc.IntentStatus,
	}).Encode()
	// Status Request is a single empty packet; pipeline it behind the handshake.
	if _, err := c.Write(append(hs, 0x01, 0x00)); err != nil {
		fmt.Println("write:", err)
		os.Exit(1)
	}

	motd, err := readStatus(c)
	if err != nil {
		fmt.Println("no status response:", err)
		os.Exit(1)
	}
	fmt.Printf("connect %v   total %v   %d bytes   claimed %q\n",
		connected.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), len(motd), claim)
	fmt.Println(motd)
}

func readStatus(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	if _, err := mc.ReadVarInt(br); err != nil { // packet length
		return "", err
	}
	id, err := mc.ReadVarInt(br)
	if err != nil {
		return "", err
	}
	if id != 0 {
		return "", fmt.Errorf("unexpected packet id %d", id)
	}
	n, err := mc.ReadVarInt(br) // JSON string length
	if err != nil {
		return "", err
	}
	if n < 0 || n > 1<<20 {
		return "", fmt.Errorf("implausible payload length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
