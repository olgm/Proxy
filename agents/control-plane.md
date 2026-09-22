# Control plane: CLI and Discord bot

Built 2026-09-04 on branch `control-plane`. Two surfaces over one whitelist, split
the way the owner asked: route settings are operator-only and live in the CLI;
Discord members manage their own whitelist entries, gated by role. Designed for more
than one entry node from the start; since 2026-09-05 all four nodes are ingresses
and all four hold the list, which needed no code change — `whitelistedEntries`
already returned every one of them, and the fan-out and the reconcile are both
written against N.

The user-facing description is in README.md ("Control link", "Discord bot"); this
is the why, and what the next change has to keep true.

## Where things run

| piece | runs on | talks to | auth |
| --- | --- | --- | --- |
| `proxyctl` | operator's machine, never a node | nodes, over ssh | ssh key with root/sudo on every node, plus `topology.json`, `tunnel-keys.json`, `.env` |
| `proxybot` | one node, `discord.node`, user `proxybot` | Discord gateway out; every entry's control link | token in `/etc/proxyd/bot.env`; every entry's control key in `/etc/proxyd/bot.json` |
| control link | proxyd, on every node holding a whitelist | - | TCP, `allow_from` the bot's node plus loopback, every frame sealed with that entry's key |

Why a node: a bot needs uptime a laptop does not have. Why one bot for every
entry: one Discord token runs in one process, so the bot has to reach the entries
it does not live on, and the only things that may cross between nodes are keyed
links. ssh keys never land on a node.

Why a link and not the file: a second writer to `whitelist.txt` either runs as the
`proxyd` user (a bot bug then reads the tunnel keys) or needs a setgid-and-chmod
dance, and either way races proxyd's own rename rewrites. Through the link proxyd
stays the single writer, validation is in one place, and the CLI uses the same path.

Rejected: ssh from the bot's node with a restricted key (a credential on a node);
nodes pulling from the bot (a stateful authority the CLI would have to write
through); proxyd-to-proxyd replication (most code, most subtle); the local Unix
socket of the first draft (a second mechanism for the one-entry case only).

## Control link

`internal/control`. Challenge, then one sealed frame each way, `[4B len][12B
nonce][AES-256-GCM]` with the challenge and a direction byte as associated data.
Random nonces are fine at human request rates; the challenge is what kills replay.
A frame that does not open gets no reply: a refusal would tell a guesser something
is listening for this shape. One key per entry, `ctl|<node>` in `tunnel-keys.json`,
same minting and reuse as the legs. mTLS was the alternative: also stdlib, but a
second key-management story with expiring certs for no gain here.

`add` resolves the missing half against Mojang on the node through the ungated
`LookupName`/`LookupUUID`; the login-path gate is for lookups a stranger can
trigger, and this path needs the key. Names are checked against
`^[A-Za-z0-9_]{1,16}$` before any lookup; legacy names outside that go in by uuid.
The bot's per-member limiter (5 adds a minute) is what bounds Mojang traffic.

## Ownership and several entries

The tag on a line is the whole ownership model: `discord:<id>`, or `cli`, or
whatever an operator wrote after `#`. proxyd keeps it through renames. The bot
keeps no state.

Every whitelisted entry carries the same membership. The primary is `discord.node`
when that is a whitelisted entry, else the first whitelisted entry in route order.
An op goes there first and stops if refused; the others follow, and an entry that
fails is named in the reply. Every five minutes the bot brings every other entry's
set of (uuid, tag) level with the primary's — a differing tag is fixed by remove
and re-add, which copies the primary's name; a level line is not touched. Names
are never compared. Without a bot, `proxyctl whitelist list` shows the divergence
and running the verb again is the fix.

## Discord

Decisions taken with the owner on 2026-09-04: a dedicated application in a server
that is not the dev server; a member's lines follow their role (pruned once they
leave or hold no listed role, checked with Get Guild Member, no privileged intent);
managers may add on a member's behalf and purge a member; private replies plus an
audit channel. Role ids, not names: names are not unique in Discord. The token
travels inside the install script over ssh stdin into a root-only file; a redeploy
without it keeps the file. `s.Identify.Intents = IntentsNone`; slash commands need
none.

The bot never prints an entry address. operational-safety.md still applies:
without the ingress per-source-IP rate limit, anyone holding an address can spend
the egress IP's reputation, and a Discord community holding it makes that limit
more urgent.

## Feeds

The bot posts the two feeds only it can build, and both are off unless their
webhook variable is set.

- **The roster** (`roster.go`) asks every entry the sessions op and joins the
  answers, because a node knows its own sessions and no others. It edits one
  message rather than reposting, and only when the roster changed — so the
  timestamp says *changed*, not *checked*. That costs the bot the first state it
  has ever kept, one message id in `/var/lib/proxybot/feeds.json`. It is not a
  source of truth: losing it costs one duplicate message, and the whitelist files
  are still the only state that decides anything.
- **The watch** (`status.go`) dials each entry's control link and each node's
  probed health link. A refused connection means the host answered and the service
  did not; silence means the node is gone. A node that did not answer is not asked
  about probed — one fault, one line. Three dials must agree before anything is
  posted, and the first observation is announced only when it is a fault.

`/watch` (`watch.go`) is the one reply that carries a client IP, so it is
managers-only and always ephemeral. Ownership stays the whitelist tag and the
sessions are matched by uuid afterwards — recording an owner against a session at
login would be a second thing that could disagree with the list. Paging asks every
node for the whole prefix and cuts the page out of the merge, because no node can
page a total order it holds only part of; the page number rides in the button's
custom id, so nothing is remembered between presses and the role is re-checked on
each one.

`probed`'s health link is a separate port and a separate key from the control
link, and exists only when `feeds.status` does. proxyd and probed still do not
know about each other; the bot is what holds both ends.

## Not done

- Per-entry membership. Every whitelisted entry carries the same list.
- Kicking a removed player mid-session.
- The ingress rate limit. Still the missing control, and more urgent now: four
  public entry addresses exist and any of them spends the one egress IP.

Verified live on 2026-09-05, four entries: an add through `proxyctl whitelist`
landed on au, ch, hk and ty, `list` showed it level across all four, and the
remove cleared all four. The lists are deliberately empty again afterwards.

The feeds went live on 2026-09-08. All four are on: sessions from every ingress,
probe from au, hk and ty, roster and watch from the bot on hk. The roster's
message id landed in `/var/lib/proxybot/feeds.json` on the first tick, which is
the end-to-end evidence that a webhook post returned a message rather than an
error; no node has logged a webhook failure since. `/watch` is registered but has
not been exercised against a real session — no player has connected since the
sessions log started, so its history pages have never had a page to turn.

## The status card

`#status` ends in a PNG and carries the transition log above it. The design
constraint that decided everything else: Discord cannot move a message, so
"always at the bottom" is not a property you can set, it is a lifecycle. Nothing
changed, edit in place; something changed, delete the card, post the line, post a
new card. The card is the only message that is ever deleted.

The quiet redraw is on a ten-minute timer and the dials stay at twenty seconds.
Those are separate on purpose: detection has to be fast, because a transition is
announced only after three dials agree, and redrawing has to be slow, because a
quiet redraw is a fresh upload of a picture that says the same thing. A
transition never waits for the timer.

It is an image and not an embed because an embed is laid out by the client
reading it, and the five columns are the whole point. Drawn in
`cmd/proxybot/card.go` at 2x from the mockup's own CSS geometry, in Go Mono from
`golang.org/x/image` — bytes in the module, so no font on the node and no `.ttf`
in the repo. That module is the one dependency added; `proxyd` and `probed` stay
dependency-free, and the go directive moved to 1.25 because x/image needs it.

Three things were not obvious while building it:

- **The state file had a latent bug.** The roster and the card both keep message
  ids in `/var/lib/proxybot/feeds.json`, and the old code wrote the whole struct
  from each. The roster saves every twenty seconds, so it would have wiped the
  card's id on the first tick and the bot would have posted a new card every
  twenty seconds forever. Every write is a read-modify-write under one lock now.
- **A latency needed the health link widened.** The measurements live in
  `probed`'s log on the node and the bot has no path to them, so `Health` carries
  the newest closed window per originating class. It is the longest window
  configured, not the shortest: the card stands for ten minutes and a one-minute
  percentile describes one of them. A class with nothing measured yet is listed
  with negative figures rather than omitted — otherwise a `probed` that just
  restarted looks exactly like ch, which originates nothing.
- **A window with 100% loss still closes, with a percentile of zero.** `Flush`
  needs `sent > 0`, not a single round trip, so a leg carrying nothing reports
  `N: 0, P50: 0`. The card would have drawn `0.0 ms` on it, the fastest-looking
  row on the board. Guard on `N`, never on the percentile.
- **Which leg a row shows is not "the longest name".** `hk>ch` and `hk>ty` both
  name two nodes; the first is the whole route through Tokyo and the second is
  its first hop. The kind decides before the length does: a chain beats a leg.

The transition wording dropped the word "unreachable" from the unreachable
condition, because the line it lands in already opens with "is offline" and the
two together read as the same word said twice.

Unverified as of writing: whether `> -# …` renders as subtext inside a quote on
every client. It is what the channel is asked for and Discord documents both
marks, but it has only been eyeballed in the desktop client.
