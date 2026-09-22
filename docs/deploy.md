# Deploy

## Requirements

**Operator machine** (wherever you run `proxyctl` from):

- Go, the version named in `go.mod` (currently 1.26.0). `proxyctl` cross-compiles
  every binary locally with `GOOS=linux CGO_ENABLED=0`, so a node never needs Go
  installed.
- An OpenSSH client (`ssh`, `scp`). `proxyctl` drives every node over ssh.

**Each node:**

- A Linux distro with systemd. The install script uses `useradd`, `systemctl` and
  `install`.
- Reachable over ssh as root, or as a user with passwordless sudo. `proxyctl` picks
  `sudo -n` for the install script unless it detects it is already root over that
  ssh target; a sudo that prompts for a password fails a non-interactive `bash -s`
  run.
- `ufw`, only if you want `proxyctl` to propose firewall rules for you. If `ufw`
  is not active — or the node uses something else — `proxyctl` reports "no active
  firewall detected" for a blocked link and stops there rather than guessing at a
  rule; you fix reachability yourself. See "Firewall" below.

**Client support:** the ingress parses one packet, the modern handshake (protocol
1.7 and up), then Login Start if a whitelist is present. A pre-1.7 client sends the
legacy `0xFE` status ping instead, which is rejected (`mc.ErrLegacyPing`) rather than
answered. Everything after Login Start is opaque bytes relayed as-is, so every
version from 1.7 onward works unchanged without proxyd knowing what it is.

## Deploy

```sh
cp topology.example.json topology.json     # edit; delete the discord block to run without the bot
cp whitelist.example.txt whitelist.txt     # edit, or drop `whitelist` from the route
cp motd.example.json motd.json             # optional; there is a built-in listing
go run ./cmd/proxyctl config               # preview, changes nothing
go run ./cmd/proxyctl deploy
go run ./cmd/proxyctl status
go run ./cmd/proxyctl whitelist list       # once deployed: manage the list from here
```

Commands: `config`, `deploy`, `status`, `uninstall`, `version`, `whitelist`. The
Discord bot needs one more thing before `deploy`; see docs/discord.md.

A deploy restarts `proxyd` on every node, which disconnects everyone playing. A
node asked to stop closes its listeners, ends each session, and waits up to five
seconds for every one to be written down and reported before it goes. So a restart
costs the players their game and costs the record nothing.

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
| `routes[].via` | ordered relay chain after the entry, ending at the exit |
| `routes[].transport` | `tcp` (default) or `udp` — see docs/transport.md |
| `routes[].duplicate` | UDP only; copies of every packet on every leg. Default 2 |
| `routes[].legs` | UDP only; `{from, to, duplicate}` per leg that should differ |
| `routes[].exit` | node every path converges on. Implied by the end of `via`; naming the entry itself is a route with no hops, where that ingress dials the target |
| `routes[].paths` | UDP only; race several ways to the exit instead of one `via` |
| `routes[].tunnel` | UDP only; sizes, deadlines and every timer — see docs/transport.md#tuning |
| `routes[].target` | final `addr`, plus `rewrite_host` / `rewrite_port` |
| `routes[].whitelist` | optional; local `ign:uuid` file seeded onto the entry node |
| `routes[].motd` | optional; local JSON file the entry answers server-list pings with |
| `discord` | optional; deploys the bot — see docs/discord.md |
| `discord.node` | node the bot runs on. Default: the first entry with a whitelist |
| `discord.guild` | the server's id |
| `discord.roles.<id>` | what a role grants: `{"accounts": N}` or `{"manage": true}` |
| `discord.audit_channel` | optional; channel that gets one line per change |
| `feeds` | optional; posts what the chain is doing to Discord — see docs/discord.md#feeds |
| `feeds.<name>.webhook_env` | environment variable holding that feed's webhook URL |
| `feeds.probe.windows` | which probe windows reach the channel. Default: the longest |
| `feeds.online.nodes` | order entries appear in the roster |
| `feeds.status.ping_role` | role id a transition pings. Default: ping nobody |

Hop ports are allocated automatically, and so is one control port per entry with a
whitelist. `config` prints the map.

## Firewall

After deploy, proxyctl tests every link from the side that will really dial it and
reports which are blocked. When a firewall is the cause it prints the exact rule and
asks before applying it; with no terminal attached it only prints. It never edits a
firewall unprompted.

The rules are one public port on the entry node, and each hop port opened to the
previous hop only:

```sh
ufw allow <entry-port>/tcp                                # entry node
ufw allow from <prev-hop-ip> to any port <hop> proto tcp  # every other node
ufw allow from <prev-hop-ip> to any port <hop> proto udp  # ... on a udp route
ufw allow from <bot-node-ip> to any port <ctl> proto tcp  # every entry the bot does not live on
ufw allow from <bot-node-ip> to any port <health> proto tcp  # probed, with a status feed
```

The entry needs nothing inbound for a UDP route: it dials out and answers come back
on the same socket. A relay or exit needs one UDP rule per path that reaches it.

A blocked UDP port cannot be found by connecting to it — that always succeeds — so
`deploy` asks the near node whether its link has been answered, and waits a few
seconds before believing it hasn't.

## DNS

`routes[].port` is whatever was free on that node, not whatever is memorable —
every entry in the examples below is on 30001. A player should not have to know
that, and does not have to: a Java client given an address with no `:port` first
looks up `_minecraft._tcp.<name>` and takes the host and port out of the SRV record
it finds. So publish one, and the address a player types is a bare hostname.

```
; one A record per node, the address the SRV points at
hk.example.com.                    300 IN A   198.51.100.10
ty.example.com.                    300 IN A   198.51.100.20
au.example.com.                    300 IN A   198.51.100.40
ch.example.com.                    300 IN A   198.51.100.30

; one SRV per name a player may type: priority, weight, port, target
_minecraft._tcp.hk.example.com.    300 IN SRV 0 5 30001 hk.example.com.
_minecraft._tcp.ty.example.com.    300 IN SRV 0 5 30001 ty.example.com.
_minecraft._tcp.au.example.com.    300 IN SRV 0 5 30001 au.example.com.
_minecraft._tcp.ch.example.com.    300 IN SRV 0 5 30001 ch.example.com.
```

`hk.example.com` in the server list then reaches `198.51.100.10:30001`. The name and
the SRV target may be the same label, as above, or the target may be a separate
`nodes.hk.example.com` — the SRV is what carries the port either way.

Four things that bite:

- **The target must be a hostname with an A or AAAA record.** Not an IP literal, and
  per RFC 2782 not a CNAME. Resolvers vary in how forgiving they are; do not rely on
  it.
- **An explicit port skips the lookup.** A player who types `hk.example.com:30001`
  gets no SRV query at all, which is the fallback if a record is wrong, and the
  reason a stale SRV can look like it works for the person who tested it.
- **Behind Cloudflare, the A record must be DNS-only** — grey cloud, not orange. The
  HTTP proxy does not carry a Minecraft TCP session, and an orange-clouded record
  hands out Cloudflare's addresses instead of the node's. SRV records are never
  proxied.
- **Bedrock clients do not do SRV.** They need the port typed into the field the app
  gives them for it. This is a Java accelerator, so that is academic here.

What the player types travels to us in the handshake, and the ingress rewrites it to
`target.rewrite_host` before anything is forwarded, when one is set — so the name
you publish is only ever seen by us, and does not have to be one the backend would
recognise. Leave `rewrite_host` empty and the client's own hostname passes through
unchanged. The published name is also the name the whole chain is reached by: an
entry is a whole path, so `au.example.com` is Sydney's route to the exit and
`ch.example.com` is the exit alone.

One SRV per entry, each naming its own node, is the shape to prefer. Pointing several
targets at one name and leaning on priority and weight to fail over is not something
Minecraft clients agree about, and a client that picks a node that is down does not
retry the next one.

## Verify

`proxyd` logs one line the moment a tunnel link starts answering, and again if one
stops, plus a line per link every 30 s with round-trip time, jitter, ping loss and
retransmit counts. That log is the only view from outside a node of whether a leg is
carrying UDP at all, and at what cost. `proxyctl status` prints the most recent line
per link beside the service state.

```
:9000: link 203.0.113.30:9001 up rtt=123.1ms mdev=0.4ms loss=0.0% sent=812 recv=790 rtx=0 dropped=0
```

`tools/mcping` sends a status ping and prints the MOTD:

```sh
go run ./tools/mcping <entry-ip>:25565
```

It answers from the ingress: it proves the entry is up, that the listing renders,
and — from the round trip it reports — what the chain currently costs, since the
ingress holds the pong for exactly that. It says nothing about the hostname
rewrite, because nothing it sends reaches the backend; a real login is the only
thing that exercises the rewrite.

Pointing it at the entry is safe for the same reason; pointing it at the backend is
still forbidden from anywhere, and from a node most of all.

Not built or deployed by proxyctl. Copy it to a node by hand if you want it there.

## Version

One version for the whole repo, in `internal/version`. Every binary reports the same
one:

```sh
go run ./cmd/proxyctl version
/usr/local/bin/proxyd -version     # on a node; likewise probed, triald, proxybot
```

```
v2.1.0 (733d62d)
```

The number is what a release is called. The revision beside it is the commit the
binary was built from, which `go build` stamps in by itself — nothing here sets it.
The two answer different questions: the number is what was announced, the revision is
what actually shipped, and only the second can tell you whether the binary on a node
is the code in your tree. A trailing `, dirty` means the tree had uncommitted changes
when it was built. `go run` does not stamp a revision, so `go run ./cmd/proxyctl
version` prints a bare `v2.1.0`; a built binary carries both.

`proxyctl status` asks every installed binary its version and prints it in each
node's heading. All of it is built from one tree by one deploy, so they should agree;
when they do not, the report ends with a `version skew` block naming which node is on
what. That is the case worth catching — a node deployed by hand, or missed by the
last deploy and still carrying the build before it.

There is no per-binary version. proxyd, proxybot, probed and triald share
`internal/tunnel`, `internal/control`, `internal/probe` and `internal/window`, so a
change to any of those moves several of them at once; separate numbers would be four
values cut from one commit that could only ever agree, kept by hand.

To cut a release: bump `V` in `internal/version/version.go`, retitle the changelog's
top section from `Unreleased` to the new number, commit, and tag that commit
`vX.Y.Z`.

## Logs

A node logs one line when a session opens and one when it closes:

```
:25565: login 203.0.113.9 name="Notch" uuid="069a79f4-…" proto=47 online=3
:25565: logout 203.0.113.9 name="Notch" uuid="069a79f4-…" for 42m18s up=4.1MB down=51.7MB chain=111.6MB online=2
```

`up` and `down` are payload: what the session carried. `chain` is what carrying it
cost the fleet in billed traffic, across every node it crossed; see the `sessions`
feed in docs/discord.md#sessions for how it is arrived at.

`name` and `uuid` come from Login Start, so they are the client's own word — see
docs/whitelist.md#what-it-does-not-do. A route with no whitelist does not read that
packet at all and logs both empty.

The exit writes a line of its own when a relayed stream ends, and it is the only
record of how the far end ended it:

```
:9006: stream 3ba40e24ed20d8f5: closed after 38m28s up=1.6MB down=37.6MB chain="eof" backend="read tcp 198.51.100.30:41022->203.0.113.5:25565: read: connection reset by peer"
```

`chain` is how the direction back toward the player ended and `backend` how the one
from the backend did: `eof` where that side reached a clean end of stream, and the
error where it did not. The entry only ever sees `tunnel: reset by peer`, which says
a stream ended early but not why — a backend that hung up and a path to it that
broke read the same there. The stream id is the same number on both nodes, so the
two accounts of one session join.

Denials, rejected handshakes and whitelist renames each log a line of their own.
`journalctl -u proxyd -f` on the node, or `proxyctl status` for the last of them.
