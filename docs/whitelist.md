# Server list

The ingress answers a server-list ping itself. It never opens the chain for one and
never forwards it, so no amount of clients refreshing their multiplayer screen —
and no scanner that finds port 25565 open — turns into traffic at the backend from
the egress address. That address is the hardest part of this system to replace, and
a status ping is the one packet a stranger can make us send without an account.

Answering locally also means the listing still works when the chain or the backend
is down, which is when a player most wants to look at it.

Two fields cannot come from the file:

- `version.protocol` is echoed from the client's handshake. Reporting our own would
  put the red "incompatible" badge on the listing for every client that is not
  exactly that version.
- `players.online` is the number of logins the ingress is relaying right now.

Everything else is whatever `routes[].motd` points at, or a built-in listing if the
route names none. The built-in default reads "Minecraft proxy" with no favicon and
no sample player. `motd.example.json` has the shape of a custom one; `favicon` is a
base64 data URI of a 64x64 PNG.

## The ping a player sees

Nothing in the status exchange carries a latency: the client times the pong itself.
Answering at once would show the distance to the ingress, which is the near end of a
chain the player's traffic has to cross all of — a flattering number, and a useless
one.

So the ingress holds the pong for as long as the rest of the chain takes. It knows
that figure because the entry times the whole tunnel: an echo goes down the hops,
each node passes it along, and the node with no hops left — the exit — turns it
around. That is our last node before the backend, so what a player reads off the
server list is the trip to the backend's doorstep, and none of it is a packet the
backend sees. The measurement repeats on the link ping interval and is smoothed the
same way a leg's own round trip is.

On a plain TCP route there is nothing measuring itself, so the pong goes out at once.

# Whitelist

Optional, and only on the entry node: it is the only hop that sees a Login Start.
One player per line.

```
# friends
Notch:069a79f4-44e9-4726-a5be-fca90e38aaf5
```

Point `routes[].whitelist` at the file and `deploy` seeds it to
`/var/lib/proxyd/whitelist.txt`, **once**. After that the node owns it: proxyd
rewrites the IGN column when a player renames, so later deploys leave it alone. Add
and remove people with `proxyctl whitelist` from your machine, through the Discord
bot, or by editing the file on the node; a change is picked up on the next login,
without a restart that would drop everyone mid-session. A file that fails to parse
leaves the last good list in place.

Anything after a `#` on a player line is that line's tag, kept through renames. The
bot writes the Discord id of whoever added the player there, and that tag is the
whole ownership model: a member may remove only lines tagged with their id.

```
Notch:069a79f4-44e9-4726-a5be-fca90e38aaf5 # discord:123456789012345678
Friend:5bc1b7b1-1c8b-4f8e-9c2e-7f2a0d3e4b5c # cli
```

Which field is checked depends on how old the client is — the UUID only exists in
Login Start from 1.19, and is only mandatory from 1.20.2:

| Client | Matched on |
| --- | --- |
| 1.20.2+ | UUID; the IGN column is corrected if the player renamed |
| 1.19–1.20.1 | UUID when sent, otherwise the IGN |
| 1.8.9–1.18.2 | IGN — those clients send no UUID at all |

Anyone not on the list gets a login Disconnect and is dropped before proxyd dials, so
they never cost the chain a connection.

## Names that moved on

The UUID column is the identity. The IGN column is only a cache of what that UUID is
called today, because names are released when a player renames and can then be
claimed by someone else. Left alone, a name written here months ago would keep
working for whoever holds it now — which matters most for 1.8.9 clients, since a name
is the only thing they send.

Two mechanisms keep that from happening, both talking to Mojang and never to the
backend, so the probe rule in `agents/operational-safety.md` does not apply to them.

**A daily refresh** re-reads every entry's current name and writes back the ones that
changed. A released name cannot be re-registered by anyone else for far longer than a
day (~37 days, **unverified**), so the cache is never stale enough for a recycled name
to be honoured. The last run is recorded in `whitelist.txt.refreshed` beside the list,
so a restart loop cannot turn into a burst of Mojang traffic. Because of this, a name
that matches the file is trusted without a lookup — logins stay local and fast.

**A lookup on a miss** covers the gap. If a client sends no UUID and its name is not
in the file, proxyd asks who owns that name right now and admits them only if the
answer is a listed UUID — then records the new name, so it is never asked again. That
is what lets a player who renamed and logged straight back in on 1.8.9 through, while
a stranger who claimed their old name resolves to their own UUID and is refused. A
login that carries a UUID never triggers a lookup: it already named an identity.

Those lookups are the only attacker-reachable outbound requests here, so they are
rate limited three ways:

| Limiter | Budget | On exhaustion |
| --- | --- | --- |
| per name | 3 / 5 min | drops to 2/hr, then 1/hr, each rung starting spent |
| per source IP | 3 / 5 min | same ladder; catches one address cycling names |
| overall | 50 / 5 min | no ladder; every lookup waits for the window |

A rung that passes with no attempt at all eases back up one, so someone who renamed
and retried a few times is not pinned for the day while sustained pressure stays
throttled. A refused lookup always denies the login — never admits it. Players who
match the list locally are unaffected by any of this.

Cost to the player when a lookup does happen, measured from the HK ingress: **~650 ms
on a cold connection, ~260 ms on a warm one**, and up to the 5 s client timeout if
Mojang is unreachable. It is paid once, because the resolved name is written to the
list, and only by someone who missed. Anyone the list already matches never waits.

The ingress is a bad place to ask from — Chicago completes the same lookup in 68 ms —
but relaying it through another node needs a control-plane protocol we do not have,
to save 400 ms on something that happens a few times a year. The measurements and the
arithmetic are in `agents/link-latency.md` if that tradeoff ever changes.

## What it does not do

**This is not authentication.** Both fields are the client's unverified word. proxyd
never terminates Minecraft's encryption, so unlike a real server it can never ask
Mojang whether a UUID belongs to the person presenting it — the session check happens
between the client and the backend, out of our sight (`agents/minecraft-protocol.md`
§4). A modified client can claim any UUID. The refresh and the miss-path lookup fix
*stale* identity, not *forged* identity: they establish which UUID owns a name today,
never that the client is that UUID.

It also does not protect the egress IP. UUIDs are public — resolvable from any IGN in
one API call — so anyone who knows that a listed player uses this proxy can get past
the gate. They cannot log in: the backend's session check fails. But they can fail it
again and again, and every attempt reaches the backend from the one egress address
all the real players share. Repeated failed session checks from a single datacenter
IP is what gets an egress address blocked, and the whitelist answers *who*, never
*how often*.

The control for that is a per-source-IP connection rate limit and concurrent cap on
the ingress — the only hop that sees a real client IP, and the one thing in the whole
exchange a client cannot forge. Not implemented.

So: the whitelist keeps uninvited players off the chain. Treat it as a door lock, not
a security boundary.

# Control link

Every entry that holds a whitelist also runs a small TCP listener through which the
list is managed from outside proxyd: by `proxyctl` over ssh, and by the Discord bot
from whichever node it lives on. `config` shows it as the `control` line. It takes
the port after the hops, and a key of its own, minted into `tunnel-keys.json` as
`ctl|<node>` beside the leg keys.

One request per connection. The node sends a random challenge, the client answers
with one frame sealed under the key with that challenge as associated data, and the
reply is sealed the same way, so a recorded exchange replays into nothing. A frame
that does not open gets no reply at all. Loopback may always connect; the bot's node
may once a `discord` block names it, and that is a TCP address, so trusting it is
sound in a way it would not be over UDP. proxyd stays the only writer of its file.

Three operations: `list`, `add`, `remove`. An add given only a name has the UUID
resolved against Mojang on the node, and the name written the way Mojang spells it;
given only a UUID, the name. A UUID already listed is refused, naming the line it
has, so nobody re-tags someone else's. On the node itself:

```sh
proxyd ctl list
proxyd ctl add Notch                # resolved against Mojang
proxyd ctl add Notch <uuid> cli     # both halves given: no lookup
proxyd ctl remove notch
```

From your machine, `proxyctl whitelist list | add <name> [uuid] | remove <name|uuid>`
runs that on **every** entry that holds a whitelist. Every such entry carries the
same list: a player is whitelisted on the chain, not on a node. `list` prints the
entries side by side and marks any line that is not on all of them; an add or remove
that one entry could not take is reported, and the fix is to run it again, or to let
the bot's reconcile settle it.

## Several entries

Two writes can never be atomic, so one entry is the **primary**: the bot's node when
that is a whitelisted entry, otherwise the first whitelisted entry in route order.
An operation goes to the primary first and stops if the primary refuses. Every five
minutes the bot lists every entry and makes each other one's set of UUID and tag
match the primary's. Names are never compared, because each node keeps its own:
renamed on login, refreshed daily, and free to differ for a day. So: hand-edit the
primary, or use the CLI or the bot. A hand edit on another entry is undone at the
next reconcile. Without a bot nothing reconciles, and the CLI's report is what tells
you an entry is behind.
