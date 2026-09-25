# UDP tunnel

`internal/tunnel`. Read before changing anything in it: most of the correctness is in
which state each node keeps, and that follows from the links, not from a role field.

## Roles are emergent, again

A node's two link sets decide everything:

| has | is | keeps |
| --- | --- | --- |
| hops, no peers | the entry | numbers chunks going out, reassembles what comes back |
| peers and hops | a relay | forwards the first copy on arrival, drops the rest, buffers both directions for retransmit |
| peers, no hops | the exit | reassembles what comes in, numbers what goes back |

`down` links point toward the exit, `up` links toward the entry. A direction
originates where its `back` set is empty and terminates where its `send` set is
empty; nothing else distinguishes the three. Do not add a role field.

## Wire

`[4B epoch][8B counter][AES-256-GCM ciphertext + 16B tag]`. Plaintext is one byte of
type then:

| type | body |
| --- | --- |
| DATA | stream u64, seq u64, flags u8 (bit 0 = FIN, bit 1 = RTX), payload |
| NACK | stream u64, count u16, count × seq u64 (cap 64) |
| ACK | stream u64, delivered-through u64 |
| HEAD | stream u64, horizon u64 (one past the sender's highest on this leg) |
| RESET | stream u64 |
| PING / PONG | nonce u64 — one leg, hop to hop |
| ECHO / ECHO-REPLY | nonce u64 — the whole chain; a node passes it on while it has hops, the exit turns it around, replies walk back up. No stream, so none is opened: the exit dials the backend on stream open, and measuring the chain must never become a connection to the backend. Only the entry originates, on the ping ticker; the first reply for a nonce wins and later copies find it gone. Feeds `Node.ChainRTT`, which the ingress reports as the server-list ping. |

Datagram ceiling is `max_datagram` (1200 default), payload therefore 1154. Nonces are
epoch + counter, never random: a 96-bit random nonce is not safe for the lifetime of
a link, and a counter also gives the replay window something to work with.

## Invariants

- **A duplicate carries the same sequence number.** That is what lets a receiver tell
  "another copy of 101" from "102 arrived, 101 did not". Renumbering a chunk anywhere
  breaks both de-duplication and gap detection.
- **Every copy is sealed separately.** Identical bytes would be dropped by the far
  end's replay window, which would silently turn `duplicate: 2` into `1`.
- **Relays do not reorder.** They forward each datagram as it lands. Reordering in
  the middle would reintroduce the head-of-line blocking the tunnel exists to avoid.
- **Every node de-duplicates; only the terminator orders.** A relay keeps the first
  copy of a number, drops the rest, and re-sends with its own link's count. The
  count is the link's, never the chunk's: `Link.dup` applies to whatever this node
  puts on that link, originated, forwarded, retransmitted or probed.
- **A re-send is not a copy.** RTX is set on NACK answers and on the tail probe. A
  relay holding that number still forwards it, because the node after it may be
  the one that lost it. Strip the bit when storing, so it does not travel further
  than the re-send that carried it.
- **Racing means every path carries every chunk.** Splitting chunks across paths
  would make each leg's sequence full of holes that are not losses.
- **ACK is cumulative and is snooped by every hop it passes**, which is how a relay
  frees its buffers. Never make it end-to-end-only.
- **A relay passes each watermark on once, so it answers for it.** The far end
  repeats its ACK every `ack_repeat_ms`, but a relay forwards one only when it
  moves. A re-send below the relay's watermark means the sender had not heard it
  when it sent: most often the copy the relay forwarded was lost, though a probe
  can also cross the ACK in flight. Either way the relay answers it with an ACK of
  the watermark; a spare one is harmless. Dropped like any other copy, the
  sender's probes go unanswered until it gives up on a stream the far end has read
  to the end.
- **A NACK for a chunk we never held becomes our own want.** That is the whole
  chained repair; without it the far end asks a node that cannot answer, forever.

## Two blind spots, and what covers them

Gap detection cannot see past the highest sequence that arrived, so it cannot notice
a lost *tail*, and Minecraft is bursty enough that the next chunk to reveal it may be
a whole tick away. Two things cover it, per leg first and end to end as a backstop:

- **HEAD, per leg.** A sender that has been quiet for `headQuiet` with chunks the
  far end has not acknowledged tells the next hop its horizon. Everything below it
  the receiver has not seen becomes an ordinary hole, NACKed on that leg. Repeated
  every leg RTO while still unacknowledged, so a lost advert costs one RTO rather
  than the tail. Every sender does this, relays included: a relay's horizon is the
  highest it has *seen*, so a hole it has itself becomes the next hop's want too.
  An end-of-burst flag on the last chunk would not do: that chunk is the one that
  was lost.
- **The probe, end to end.** The originator re-sends its **highest** chunk when
  nothing is being acknowledged, which moves the receiver's horizon the same way.
  Re-sending the lowest unacknowledged chunk instead recovers a tail at one chunk per
  timeout, which is fast enough to look like it works and slow enough to stall a
  session under real loss. The probe has to cross every relay to work — that is what
  the RTX flag is for — because the leg that lost the tail may be the last one.
  It is also how the originator learns the path is gone: it gives up after 8
  unanswered, and never before `repair_ms` without progress. The count alone is
  spent in under a second on a fast path, while the far end is still repairing.

Nothing distinguishes a quiet session from a dead path either. Link liveness does,
but only as half the test: a link is called up on a returned pong, so a node that has
just started has none, and a node whose peer has never spoken cannot even ping. A
stream is given up only when no link has answered *and* that stream itself has
received nothing for a repair window. Either half alone kills live sessions in the
second after a restart.

## Timers

All in `Options.Timers`, set from the `tunnel` block of the config, defaults in
`defaultTimers`. Do not add a new interval as a constant: put it there, so it can be
tuned per route without a rebuild.

| what | config | default |
| --- | --- | --- |
| tick | `tick_ms` | 5 ms |
| ping | `ping_ms` | 1 s; also the entry's NAT keepalive, since it never binds |
| link down | 5 × ping | 5 s |
| NACK re-ask | `nack_min_ms`–`nack_max_ms` clamp on that leg's `srtt + 4·mdev` | 10 ms – 1 s |
| ACK | `ack_ms` on advance; `ack_repeat_ms` otherwise | 20 ms; 250 ms |
| HEAD | `head_quiet_ms` after the last send with chunks unacknowledged; then the NACK re-ask interval | 10 ms |
| tail probe | `probe_min_ms`–`probe_max_ms` clamp on end-to-end `srtt + 4·mdev`, doubling; gives up after 8 tries and `repair_ms` without progress | 100 ms – 1 s |
| give up on a hole | `repair_ms` | 5 s |
| closed stream lingers | fixed | 60 s, answering late NACKs and refusing to be re-opened |
| relay stream idle | `idle_ms` | 120 s |
| link stats line | fixed | 30 s |

The linger matters: without it a retransmitted chunk arriving after a stream closed
would read as a brand new stream, and the exit would dial the backend again. That is
one more connection to the backend from the egress address per stray copy.

The linger only covers a stream the exit still remembers. One it has forgotten — it
restarted, or the stream ended longer ago than the linger — is covered by the other
rule: **the exit hands a stream to `Accept`, and so dials, only once chunk 0 is in
hand.** A chunk from further in creates state and asks for the missing start like
any other hole, so a start that merely arrived late opens the stream the moment it
lands. When nobody can supply it, the hole outlives the repair window, the stream is
reset, and the reset tells the chain. Before this, a restarted exit dialled the
backend for the next chunk of every session it had been carrying.

A stream that ends is accounted for at both ends, and only together do the two say
what happened. The entry logs `tunnel: reset by peer` when a RESET arrives, which
carries no reason from the far end by design. The exit logs `closed after … chain=…
backend=…`, where the backend field is the error its own TCP connection ended with —
`eof` for a clean FIN, `connection reset by peer` for an RST, a timeout for a path
that stopped answering. Join them on the stream id. Before v2.3.4 `relay` discarded
those errors, so a fleet-wide drop could not be charged to the backend or to transit;
see the 2026-09-14 08:06:27Z incident, where the distinction had to be inferred from
how quickly reconnects succeeded.

## Handoff

A node can be frozen in one process and resumed in the next without the chain
noticing (`handoff.go`). The sockets move as they are, so no neighbour sees a port,
address or key change, and so does everything a socket does not hold: every
stream's buffers, sequence numbers and round trips, each leg's learned address and
replay window. What arrives in between waits in the socket; what the old process
never sent is found by the HEAD advert the new one sends at once, and by NACKs.

- **Halt before freeze.** `Stream.Halt` stops the layer above at a byte boundary:
  Read, Write and CloseWrite return `ErrHalted`, Close does nothing. Only once
  nothing above is touching a stream does `Node.Freeze` stop the readers and the
  timers and write the state down. From then on the node sends nothing at all.
- **The seal does not move.** The new process starts a fresh epoch, as any restart
  does; carrying the counter would make every nonce's uniqueness depend on the
  freeze having stopped every sender first. Peers already treat a new epoch as a
  restart.
- **Streams nobody claims are settled, not dropped.** `Resume.Attached` names the
  streams the layer above carries on with. At the exit, one of the rest that
  nothing ever read was frozen before anyone dialled, and is offered to `Accept`
  again. One that something did read had a relay, which ended or was lost in the
  handoff: offering it would dial the backend for the tail of a stream, so it is
  closed, and reset unless it had reached its end. At the entry they are all
  reset, since nothing is left to write them.
- **Only the exit offers.** The entry opens its own streams and nothing there
  reads `Accept`, so a reply arriving on one must never queue it: 64 of them fill
  the backlog, and `offer` then drops the live stream from the node.

## Testing

`tunnel_test.go` puts a UDP relay between real sockets and drops datagrams on demand,
so loss is injected on the wire rather than through a seam cut into the code. Keep it
that way — the interesting bugs are in the socket and timer paths a fake would skip.
