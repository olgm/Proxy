# Operational safety

Rules from real incidents. The egress address is the hardest part of this system to
replace, and a backend's blacklist duration is unknown and assumed long.

## Hard rules

| Rule | Detail |
| --- | --- |
| **Never probe the backend from an acceleration node.** | No ICMP, TCP connect, status ping, `mcping`, or traceroute. Applies to every node, not just the egress. An egress address that probed a large server saw its status latency go erratic within the hour. |
| The data path does not probe it either. | A server-list ping stops at the ingress and is answered there; the chain times itself with the tunnel's own ECHO, which the exit turns around. Neither reaches the backend. Never add a status pass-through. |
| Probes from our own devices only, and sparingly. | A few pings to establish a baseline. Not loops, not repeated status queries. |
| DNS is fine. | `dig` / `getent` never contacts the backend. |
| Node-to-node measurement is fine. | AU/HK↔TY↔CH stays inside our own infra. `probed` is exactly this, continuously, and only between pairs the routes already use. |

Real player traffic through the chain is not a probe. Synthetic test traffic that
terminates at the backend is, no matter which end it was launched from.

Measuring the chain: test the hops against each other, or against a backend that
isn't the production one. A live login by a real player is the only acceptable
end-to-end check.

## mcping and tcpping

`mcping` lives in `tools/`. Run it from your own machine. Pointing it at the
backend breaks the rule above and always will.

Pointing it at the chain entry is safe: the ingress answers status pings itself,
so nothing it sends reaches the backend. Still do not install it on a node — it
has no reason to run there, and what it no longer proves is the hostname rewrite:
nothing it sends gets past the ingress, so only a real login exercises that path.

`tcpping` is a generic TCP dialer, so the hard rule applies to it too — never aim
it at the backend. Aiming it at the chain entry is harmless: the ingress accepts,
waits for a handshake that never comes, and closes after ten seconds without
dialling anything.

## What lives on a node

Every node carries `proxyd`. The node named by `discord.node` also carries
`proxybot`. A node on a topology whose config carries a `probe` block also carries
`probed`, which reaches its own peers and, when a probe feed is configured, one
Discord webhook — no Mojang, and never the backend. `triald` runs only while a
trial is armed, on whichever nodes that trial names, and comes off again once the
question it exists to answer is answered.

`probed` is covered by the hard rule above and satisfies it by construction: its
only peers are the nodes named by the routes, a chain probe is turned around at
the exit exactly as the tunnel's ECHO is, and there is no code path that forwards
past a node with no hop left.

`proxybot` talks to Discord, Mojang (through proxyd) and the entries' control
links, and nothing else; it holds no way to reach the backend.

## Whitelist, and what it does not cover

The ingress whitelist (`README.md`, "Whitelist") keeps uninvited players off the
chain. It does **not** protect the egress address, and must not be recorded as if
it does.

It matches an IGN and UUID the client asserts in Login Start. Nothing here can
verify that assertion — proxyd never holds the encryption keys, so it never learns
what the backend concluded (`minecraft-protocol.md` §4). UUIDs are public: one
Mojang API call turns any IGN into one. So anyone who knows that a listed player
uses this proxy can pass the gate, and will then fail the backend's session check
as "Failed to verify username".

That failure is the dangerous part. It arrives at the backend from the one address
every real player shares whichever ingress they entered at, and repeated failed
session checks from one datacenter address is exactly the pattern egress addresses
get blocked for. An attacker can repeat it as fast as they like; the whitelist
decides *who*, never *how often*.

One class of that is now closed: a status ping is answered on the ingress and
never forwarded, so the cheapest way to spend the egress address's reputation —
open a TCP connection, send a handshake with intent 1, repeat — no longer reaches
the backend at all, and needs no whitelist entry to attempt. A login attempt
still does.

Every entry can carry a memorable name a player types instead of a bare address
(README.md, "DNS"). A name is easier to pass around than a bare address, which
makes the missing rate limit below more urgent, not less.

**Not implemented:** a per-source-address connection rate limit and concurrent cap
on the ingress. It has to be the ingress — that is the only hop that sees a real
client address, and the source address is the one field in the exchange a client
cannot forge. Until it exists, treat an entry's address as something to share
narrowly. Anyone who learns it can spend the egress address's reputation,
whitelist or not.

## Secrets

`.env` at the repo root, gitignored, mode 600. Holds a hosting-provider API key if
you use one, once the bot exists `DISCORD_BOT_TOKEN`, and one webhook URL per feed.
A webhook URL is a bearer credential — anyone holding one can post as the bot — so
`topology.json` names the variable and never the URL. Never echo them, never paste
them into a transcript, never pass them as a shell argument that lands in logs.
Source it (`set -a; . ./.env; set +a`) and read them from the environment.
`proxyctl deploy` carries each of them to its node inside the install script over
ssh's stdin, and those files are the only place they exist off this machine:

| file | holds | on |
| --- | --- | --- |
| `/etc/proxyd/bot.env` | `DISCORD_BOT_TOKEN` | the bot's node, 0600 root |
| `/etc/proxyd/bot-feeds.env` | the online and status webhooks | the bot's node |
| `/etc/proxyd/feeds.env` | the sessions webhook | every ingress |
| `/etc/probed/feeds.env` | the probe webhook | every probe originator |

One file per service, so a redeploy run without the token in the environment
cannot clobber the feeds, or the reverse. Deleting a feed from `topology.json`
removes its file, which is what makes turning a feed off actually turn it off.

If you use a hosting-provider API key for automation, prefer a scoped key over one
with full account access, and set its own IP allowlist.
