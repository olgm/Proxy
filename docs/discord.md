# Discord bot

`proxybot` lets members of one Discord server manage their own whitelist entries,
within what their roles allow. It runs on one node — `discord.node`, by default the
primary — and reaches every entry over its control link, so it keeps no state of its
own: the whitelist files are the truth, and the tags in them say whose line is whose.

Setting it up, once:

1. [discord.com/developers](https://discord.com/developers/applications): New
   Application → Bot → Reset Token, and keep the token. Under OAuth2 → URL Generator
   pick the scopes `bot` and `applications.commands`, no permissions, open the URL
   and invite it. The bot needs no privileged intent and no channel permission.
2. In Discord, Settings → Advanced → Developer Mode. Right-click the server, each
   role that should count, and the audit channel if you want one, and Copy ID.
3. Fill the `discord` block in `topology.json` with those ids. A role grants either
   an account cap or manage; a member with several roles gets the highest cap, and
   manage if any of them says so. Members with none of the listed roles cannot use
   the bot.
4. Put the token in `.env` as `DISCORD_BOT_TOKEN=...`, then `set -a; . ./.env; set +a`
   and `proxyctl deploy`. The token travels inside the install script over ssh into
   `/etc/proxyd/bot.env`, root-only, read by the unit; it is never on a command line
   and never in `/tmp`. A later deploy without it in the environment keeps the file.

Commands, all replying privately to whoever ran them:

| command | who | does |
| --- | --- | --- |
| `/whitelist add <ign>` | any listed role, up to its cap | lists the account under your id, on every entry |
| `/whitelist add <ign> user:@x` | managers | lists it under that member's id |
| `/whitelist remove <player>` | the line's owner; managers, any line | drops it everywhere |
| `/whitelist list` | anyone: own lines. managers: every line, with owners | |
| `/whitelist list user:@x` | managers | that member's lines |
| `/whitelist purge user:@x` | managers | drops every line that member owns |
| `/watch user:@x` | managers | that member's accounts, open sessions and session history |

`/watch` answers privately, with a page of five finished sessions and a button for
the next:

```
**Watching @someone**

**Accounts**
`Notch` `069a79f4-44e9-4726-a5be-fca90e38aaf5`

**Online now**
`Notch` on **hk** from `203.0.113.9` · 12m

**Sessions** — page 1
`Notch` **hk** `203.0.113.9` · 5 Sep 11:00 → 11:42 (42m) · up 4.1MB down 51.7MB · chain 111.6MB · rtt 33 ms, p90 36 · Seoul, KR · AS4766 Korea Telecom
```

**This is the one place the client IP is reported**, and why the command is
managers-only and the reply is always ephemeral. One manager reading a private
reply is a different audience from a channel, which is why the `sessions` feed
does not carry it. The same goes for where the player's network is.

`rtt` is the player's own round trip to the entry, median and p90 over the
session; the logout line in the node's journal has the rest (docs/deploy.md#logs).
The place and network are there only on an entry with `"ipinfo": true`. A session
recorded before either was measured shows neither.

Which accounts belong to the member is decided by the tag on their whitelist
lines, as everywhere else; the sessions are then found by uuid. So `/watch` shows
the history of the accounts they own **now** — hand an account to somebody else
and its past sessions follow the account, not the member who used to hold it.

A client before 1.19 sends no uuid in Login Start, so the node takes the one the
whitelist matched it to and records the session under that.

Sessions come from `/var/lib/proxyd/sessions.jsonl`, which every ingress writes as
a session ends. It holds what the node's journal line already holds. It is bounded
by size rather than by age — 64 MB with one previous file kept — so on a busy node
the oldest sessions fall off sooner than on a quiet one. Worth knowing before
relying on it for anything.

A cap counts the lines tagged with the member's id. Managers have no cap. A name
nobody holds, a name Mojang could not be asked about, and an account someone else
already listed are each told apart in the reply.

The whitelist follows the role. Every five minutes the bot asks Discord about each
member who owns a line — one Get Guild Member call each, which needs no privileged
intent — and drops the lines of anyone who left the server or no longer holds any
listed role. A member Discord could not be asked about keeps their lines until it
can. A cap that was lowered only blocks new adds. Every change the bot makes is in
the node's journal, and in the audit channel when one is set.

`proxyctl status` reports the bot beside the nodes. `proxyctl uninstall` removes it,
including the token file.

What the bot does not change: the whitelist is still a door lock and not
authentication, the entry address is still something to hand out narrowly until the
ingress rate limit exists, and the bot never prints that address. Anyone with Manage
Roles in the server can hand out a listed role, which is Discord's trust model, not
ours.

## Feeds

A `feeds` block posts what the chain is doing to Discord webhooks. Every feed is
off until it is named there, and the chain is unchanged without one.

```json
"feeds": {
  "sessions": {"webhook_env": "DISCORD_SESSIONS_WEBHOOK"}
}
```

A feed names an **environment variable**, not a URL. A webhook URL is a bearer
credential — anyone holding it can post into that channel — and `topology.json`
is the file you edit and paste at people. Put the URL in `.env` beside the bot
token, `set -a; . ./.env; set +a`, and deploy. It travels inside the install
script over ssh stdin into a root-owned env file the unit reads, so it is never
on a command line and never in `/tmp`. Two feeds naming the same variable land in
the same channel; that is how you keep one channel or four.

`proxyctl config` lists which feeds are on and which nodes post them, and never
prints a URL. Deleting a feed removes the file on the next deploy, so turning one
off turns it off.

Who posts a feed is not a setting, because it follows from who can see it:

| feed | posted by | on |
| --- | --- | --- |
| `sessions` | `proxyd` | every ingress, about its own node only |
| `probe` | `probed` | every node that originates a class |
| `online` | `proxybot` | one message, edited in place, across every entry |
| `status` | `proxybot` | a card at the foot of the channel, and the transitions above it |

### sessions

One line when a player logs in and one when they log out:

```
**hk** `Notch` joined `069a79f4-44e9-4726-a5be-fca90e38aaf5` · online 2
**hk** `Notch` left · 42m18s · up 4.1MB down 51.7MB · chain 111.6MB · online 1
```

`online` is that node's own count, not the fleet's. A node knows its own sessions
and no others — nothing crosses between nodes but keyed links, and a login is not
worth one.

It is also that *listener's* count, while the roster `/watch` reads belongs to the
node. Every node deployed today has exactly one Minecraft listener, so the two
always agree; give a node a second ingress and the feed would report one number
and the roster another, each right about a different question.

`up` and `down` are payload: what the session carried, each way. `chain` is what
carrying it cost the fleet in traffic a VPS bills for — every byte in or out of
every node that touched it. All three are bytes, and nothing here is a ratio: the
figure is meant to be added up across a month against what the boxes allow, and
how many times the payload it happens to be is arithmetic a reader can do when
they want it.

A byte crossing a tunnel leg is billed twice, once leaving one node and once
arriving at the next, and the two ends of the chain are billed once each, where
the far side is a player or the backend. So a session over a direct exit costs
exactly twice its payload, which is the floor, and every duplicated leg adds to
it. Comparing `chain` against `up` plus `down` is the only way to see what a
`duplicate` setting is actually buying.

Each node measures its own leg exactly — duplicates, re-sends, and the acks and
nacks that repair them. The legs past it are reckoned to cost the same, which
holds while every leg carries the same chunks the same number of times, and is
out by however much their loss rates differ. Link keepalives are not counted: a
ping belongs to the leg whether anyone is playing or not.

An account holds one session. A client that reconnects while its last attempt is
still open ends that one first, so a retry replaces a session rather than adding
a second under the same name.

`name` and `uuid` come from Login Start, so they are the client's own word — see
docs/whitelist.md#what-it-does-not-do.

**The client IP is deliberately not here.**  It is in the node's journal, where an
operator who has the node already has it. A channel is a wider audience than that,
and a login feed already tells its readers which entries are live, so point it at
a channel you would not hand the entry addresses to.

A burst of reconnects arrives as one message rather than fifty: lines are gathered
for a couple of seconds and posted together, and sooner than that if the queue is
filling up. If more arrive than it holds, the feed says how many it dropped rather
than blocking the login — a feed that stalls a session is worse than a feed with a
hole in it, and one with a hole it does not mention is worse than either.

### probe

One line per class when a window closes, from the node that measured it:

```
**hk** `hk>ty` leg ×1 · 10m · p50 44.1ms p90 44.4ms p99 45.9ms mdev 0.31 · loss 0.00% (out 0.00 back 0.00) · n 600
**hk** `hk>ty` leg ×2 · 10m · p50 44.0ms p90 44.2ms p99 44.8ms mdev 0.28 · loss 0.00% (out 0.00 back 0.00) · n 600
```

Those two lines are the measurement the tool exists to make: the same leg at one
copy and at the count it really carries, in the same window. `out` and `back`
split the loss into the direction that dropped it, and are absent rather than
zero on the first window of a series — the count that arrived is the difference
of two counters, and the earlier one comes from the window before.

`windows` picks which of the `probe` windows reach the channel; it changes
nothing about what is measured or logged, and the dataset keeps every window
either way. The default is the longest one configured, which is the only one
whose p99 has enough samples behind it to mean anything. Without a filter the
two classes Hong Kong originates would post four messages a minute.

Only a node that originates a class posts. Chicago answers everything and
measures nothing, so it is never given the URL.

This is the one change that widens what `probed` talks to. It reaches its own
peers, and now a webhook. It still never touches the backend, and the hard rule
in `agents/operational-safety.md` is unchanged.

### online

One message the bot keeps up to date, grouped by the entry each player arrived
at:

```
**Online — 4**

**au** `jeb_` 1h30m
**hk** `Notch` 12m · `Herobrine` 3m
**ty** `Dinnerbone` 22m

_changed <t:1757087460:R>_
```

`nodes` sets the order the entries appear in; anything not named falls to the end,
so an entry added later shows up rather than disappearing. An entry with nobody on
takes no line, and one that could not be reached says `unreachable` — that is not
the same fact as nobody being online there, and the count in the header never
includes it.

The message is edited rather than reposted, and only when the roster actually
changed. That is why the timestamp says *changed* and not *checked*: a roster that
has not moved in an hour is still a correct one, and Discord counts the relative
timestamp up on its own without anything being edited.

Only the bot can build this. A node knows its own sessions and no others, so it
asks all five over their control links and joins the answers.

To do that it keeps a little between restarts in `/var/lib/proxybot/feeds.json`:
the ids of the messages it edits in place, and any fault it has announced and
not yet seen recover. It is not a source of truth for anything: losing it costs a
duplicate message and a repeated alarm, and the whitelist files remain the only
state that matters.

### status

One channel with two things in it: a card at the foot showing the whole chain at a
glance, and above it the log of what changed to get there.

```
> -# `ty` is offline — probed is down
> -# `ty` is online
`ch` is offline — no answer from the node at all
[ the card ]
```

The bot posts both, because a node that is down cannot report that it is down.

**The card is always the last message.** With nothing to report it is edited where
it stands every ten minutes, refreshing the latencies and the clock in its corner.
Nothing is waiting on that timer: anything worth knowing is a transition, and a
transition redraws the card immediately. When something does change the card is
deleted, the transition is posted, and a new card goes up underneath it — which is
the only way Discord will keep it at the foot, since a message cannot be moved. It
is a PNG rather than an embed: an embed is laid out by whichever client is reading
it, and the columns that make the card scannable collapse on a phone into a
paragraph nobody reads. It is drawn in `cmd/proxybot/card.go` in Go Mono, which
`x/image` ships as bytes, so nothing has to be installed on the node.

**A transition is posted once and then quietened.** A fault stands in full for as
long as it is open. When the node recovers, the recovery is posted and both halves
of the incident — the line that raised it and the line that closed it — are edited
down to `> -# …`, which Discord draws small and grey. An incident that is over
should not look like one that is not. Editing never notifies anyone, so the ping
that went with the original is not repeated.

`feeds.status.ping_role` names a Discord role id that a transition mentions. Unset,
nothing is ever pinged, which is the default. That role is the only mention this
feed can ever make: everything else stays suppressed, including anything inside a
fault that happens to look like one.

Every 20 seconds the bot dials each entry's control link and each node's `probed`
health link. A **refused** connection is the kernel saying the host is there and
nothing is on that port, which is a service being down; **silence** is the node
itself being gone. Those are different faults and never read the same. A node that
did not answer at all is not then asked about `probed`, so one fault is one line.

A change has to hold for three dials — about a minute — before anything is
posted, so a single dropped packet is not an outage. A fault that is already there
when the bot starts is announced immediately, because a node that was down before
the bot came up is still news — unless it was already announced before a restart,
which the state file remembers so a redeploy does not repeat every open fault.

Turning this feed on gives `probed` one more thing: a small TCP port per node,
sealed under a key of its own. It answers "I am running, with N classes", and the
newest closed window for each class the node originates — which is where the card's
latencies come from, and the only way to a measurement without shipping the dataset
off the node. It is the **longest** window configured, the same one the probe feed
posts: a shorter one is fresher at the moment the card is drawn, but it describes a
minute and the card describes the ten it will be sitting there for. The cost is
that a freshly deployed `probed` shows no latency until its first long window
closes.

A class that has measured nothing yet is listed with negative figures rather than
left out, so an exit that originates nothing can be told from a `probed` that has
just restarted. Nothing is drawn for a window with no round trips behind it either:
a window in which every probe was lost still closes, and its percentile is zero,
which against a leg that carried nothing is the most wrong a latency can be. A
window more than three windows old is not shown at all: a stopped `probed` keeps
reporting its last one forever, and the card would rather say nothing than say
something that stopped being true. `deploy` allocates
the port, opens it to the bot's node only, and verifies it like any other link.
Without `feeds.status` there is no port, because nothing would ever dial it. The
key is `probed`'s and deliberately not the node's control key: holding it is not a
way into a session.
