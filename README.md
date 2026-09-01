# proxy

Minecraft TCP accelerator. Chains relay nodes between player and server so every hop
runs its own congestion control — loss on one leg doesn't stall the rest.

Targets Hypixel. Deployed today as HK ingress → Tokyo relay → Chicago egress.

## Design

One binary (`proxyd`) on every node. A node has no role field: it is a list of
listeners, each with one upstream. A listener with a `minecraft` block is an ingress;
one without is a relay. A second ingress is one more listener, not a code change.

The ingress parses exactly one packet — the handshake — rewrites the address to what
Hypixel expects, then relays raw bytes via `splice(2)`. It cannot do more: the client
encrypts from Encryption Response onward.

## Deploy

Needs Go and ssh locally; root or passwordless sudo on each node.

```sh
cp topology.example.json topology.json   # edit
go run ./cmd/proxyctl config             # preview, changes nothing
go run ./cmd/proxyctl deploy
go run ./cmd/proxyctl status
```

Commands: `config`, `deploy`, `status`, `uninstall`.

## topology.json

| field | meaning |
| --- | --- |
| `user` | unprivileged account proxyd runs as. Never root. |
| `base_port` | first auto-allocated hop port |
| `nodes.<n>.ssh` | ssh target or `~/.ssh/config` alias (control plane) |
| `nodes.<n>.addr` | address the previous hop dials (data plane) |
| `nodes.<n>.bind_addr` | optional; bind listeners to one local address only |
| `routes[].entry` | node clients connect to |
| `routes[].port` | public port on the entry node |
| `routes[].via` | ordered relay chain after the entry |
| `routes[].target` | final `addr`, plus `rewrite_host` / `rewrite_port` |

Hop ports are allocated automatically. `config` prints the map.

## Firewall

After deploy, proxyctl tests every link from the side that will really dial it and
reports which are blocked. When a firewall is the cause it prints the exact rule and
asks before applying it; with no terminal attached it only prints. It never edits a
firewall unprompted.

The rules are one public port on the entry node, and each hop port opened to the
previous hop only:

```sh
ufw allow 25565/tcp                                       # entry node
ufw allow from <prev-hop-ip> to any port <hop> proto tcp  # every other node
```

## Verify

`tools/mcping` sends a status ping and prints the MOTD. It claims a wrong hostname by
default, so a reply proves the ingress rewrote the address rather than the client
having asked for the right thing:

```sh
go run ./tools/mcping <entry-ip>:25565
```

Not built or deployed by proxyctl. Copy it to a node by hand if you want it there.

## Measure

`tools/tcpping` times the SYN → SYN-ACK exchange. A network can rate-limit or
deprioritise ICMP independently of TCP, so an ICMP number is not the number proxyd
sees — on one of our legs the two differ by 3 ms, and only on one address family.

```sh
go run ./tools/tcpping -listen :19999          # target, on the far node
go run ./tools/tcpping -c 100 <host>:19999     # probe, from the near node
```

Point it at a mesh address to time a tunnel or a public address to time the raw path;
the destination picks the route, `-4` / `-6` pins the family. Same caveat as `mcping`:
never aim it at the backend from a node.

## Notes

- Relays enforce a source-IP allowlist in-binary. Without it a relay is an open proxy
  to Hypixel and the abuse lands on your egress IP.
- Moving a hop onto a mesh VPN is an address change in `topology.json`, not a code
  change. Measured on the HK→TY→CHI chain, Tailscale and public IPv4 land within
  0.5 ms of each other end to end, with Tailscale the steadier of the two. That holds
  only once the tunnel's underlay is pinned to IPv4: left to choose, it took an IPv6
  path on one leg that cost 2.5 ms and 16x the jitter. Default stays public IPv4, for
  one less dependency rather than for speed.
- Egress IP quality matters: Hypixel blocks flagged datacenter ranges. Don't probe the
  backend from an egress node either (pings, status queries, benchmark loops); test
  from your own machine, and treat the egress IP as something you can't easily
  replace.
