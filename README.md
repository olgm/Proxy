# proxy

Minecraft TCP accelerator, for any Minecraft server, 1.7+. Chains relay nodes
between player and server so a lost packet is replaced by the node before it
rather than by the far end.

Legs run over plain TCP or over a UDP tunnel, per route. The tunnel can send every
packet more than once, and down more than one path at a time. A topology can be as
small as a single node dialing the backend, or as large as a chain of relays.

## Design

One binary (`proxyd`) on every node. A node has no role field: it is a list of
listeners, each with one next leg. A listener with a `minecraft` block is an ingress;
one that dials the target is an exit; anything else passes bytes along. A second
ingress is one more listener, not a code change.

The ingress parses exactly one packet — the handshake — rewrites the address to what
the backend expects, then relays raw bytes: as they are on a TCP route, as numbered
chunks on a UDP one. It cannot do more: the client encrypts from Encryption Response
onward.

A deploy does not disconnect anyone. The running `proxyd` hands its sockets and
every session's state to systemd's fd store and the new binary carries on from the
same bytes. See docs/deploy.md#deploying-under-players.

A server-list ping is the exception: the ingress answers it itself and never opens
the chain for one. See docs/whitelist.md#server-list.

## Quick start

```sh
cp topology.example.json topology.json     # edit; delete the discord block to run without the bot
cp whitelist.example.txt whitelist.txt     # edit, or drop `whitelist` from the route
cp motd.example.json motd.json             # optional; there is a built-in listing
go run ./cmd/proxyctl config               # preview, changes nothing
go run ./cmd/proxyctl deploy
go run ./cmd/proxyctl status
go run ./cmd/proxyctl whitelist list       # once deployed: manage the list from here
```

The Discord bot (docs/discord.md) is an optional add-on; the steps above run
without it.

## Requirements

Operator machine: Go (the version in `go.mod`) and an OpenSSH client. Each node:
a systemd Linux distro, reachable as root or with passwordless sudo; `ufw` only if
you want `proxyctl` to propose firewall rules for you. Binaries are cross-compiled
locally, so nodes never need Go installed. Full detail, including what client
versions are supported: docs/deploy.md.

## Docs

| doc | covers |
| --- | --- |
| [docs/deploy.md](docs/deploy.md) | requirements, deploy, `topology.json`, firewall, DNS, verifying a deploy, versioning, logs |
| [docs/transport.md](docs/transport.md) | the UDP tunnel: how it works, duplication, racing, tuning, keys, and measuring raw path latency |
| [docs/whitelist.md](docs/whitelist.md) | the server-list ping, the whitelist, and the control link that manages it |
| [docs/discord.md](docs/discord.md) | the Discord bot and the feeds it and `proxyd`/`probed` post |
| [docs/probe.md](docs/probe.md) | `probed`, the always-on latency and loss measurement service |
| [docs/trial.md](docs/trial.md) | `triald`, the temporary bake-off harness for candidate nodes |
| [agents/AGENTS.md](agents/AGENTS.md) | design notes for anyone changing the code |

## Notes

- TCP relays enforce a source-IP allowlist in-binary. Without it a relay is an open
  proxy to the backend and the abuse lands on your egress IP. UDP hops cannot use
  one — the address is forgeable — so they authenticate every datagram instead.
- UDP is not uniformly welcome. China-route and other cheap transit commonly polices
  or deprioritises it, so a leg can be slower on the tunnel than on TCP even with no
  loss at all. `transport` is per route: run both and compare the link lines before
  committing.
- Moving a hop onto a mesh VPN is an address change in `topology.json`, not a code
  change. Pin the tunnel's underlay to IPv4: left to choose its own path, a mesh
  can pick an IPv6 route on one leg that costs materially more in latency and
  jitter than the IPv4 one. Default stays public IPv4, for one less dependency
  rather than for speed.
- Egress IP quality matters: large servers, Hypixel among them, block flagged
  datacenter ranges. Don't probe the backend from an egress node either (pings,
  status queries, benchmark loops); test from your own machine, and treat the
  egress IP as something you can't easily replace. Nothing in the data path
  probes it either: a server-list ping stops at the ingress and the chain times
  itself with its own echo.

## License

MIT. See [LICENSE](LICENSE).

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to change the code, and
[SECURITY.md](SECURITY.md) for how to report a problem.
