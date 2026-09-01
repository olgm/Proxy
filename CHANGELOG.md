# Changelog

## Unreleased

- `proxyd`: config-driven relay node. Handshake rewrite (Forge markers preserved,
  BungeeCord identity stripped), source-IP allowlist, `splice(2)` relay with
  per-direction half-close, legacy `0xFE` ping rejected.
- `proxyctl`: topology-driven deployer over ssh/scp. Derives every node config from
  one file, cross-compiles per node arch, installs a hardened systemd unit running as
  an unprivileged account.
- `proxyctl deploy` verifies every link after installing, probes the target's
  firewall when one is blocked, and offers to open exactly that port. Never edits a
  firewall without being asked.
- `proxyd`: optional UUID whitelist on the ingress. Reads Login Start — the last
  plaintext packet naming the player — and drops anyone not on an `ign:uuid` list,
  with a login Disconnect rather than a silent close. Checked before dialing, so a
  stranger never costs the chain a connection or reaches Hypixel from our egress IP.
  Matches on UUID where the client sends one, on IGN for clients before 1.19 that
  send none, and rewrites the stored IGN when a player renames. The list reloads on
  change, so edits need no restart. Status pings are not gated.
- **The whitelist is not an authentication boundary.** Both fields are the client's
  unverified word: proxyd never terminates Minecraft's encryption, so it can never
  ask Mojang whether a UUID really belongs to that player. Anyone who knows a listed
  UUID — they are public, resolvable from any IGN — passes the gate, fails Mojang's
  check at Hypixel, and can repeat that at any rate they like. Failed session checks
  from one datacenter IP is the signature Hypixel bans egress addresses for, and the
  whitelist does nothing to slow it. **The control for that is a per-source-IP
  connection rate limit and concurrent cap on the ingress**, which is the only hop
  that sees a real client IP; not implemented.
- `tools/mcping`: status-ping client for checking a chain end to end. Outside the
  deploy path; build and copy it by hand.
- `tools/tcpping`: TCP round-trip timing, with a `-listen` mode so any node can be a
  target. Measures what proxyd carries instead of what ICMP reports. Outside the
  deploy path.
- Measured a candidate second Tokyo node (ty-a `nrt`, `vc2-1c-1gb`): 20 ICMP echoes
  from each existing node, 0% loss, HK 44.6 ms avg and Chicago 136.1 ms avg. Node was
  provisioned and destroyed on 2026-09-01; no topology refers to it and its IP is back
  in ty-a's pool.
- Pinned the Tailscale underlay to IPv4 on the HK↔Tokyo link, which had negotiated an
  IPv6 path costing 2.5 ms avg and 8.3 ms mdev against 0.5 ms on IPv4. Tokyo↔Chicago
  needed nothing: Chicago has no public IPv6. With both legs on IPv4 the chain is
  167.5 ms p50 over Tailscale against 167.9 ms over public IPv4, so transport is now a
  wash and the choice is about dependencies, not latency.
- Verified HK → Tokyo → Chicago → Hypixel: status ping returns Hypixel's MOTD while
  the client claims an unrelated hostname, and all three hops appear in the socket
  table. Real client login through the chain confirmed working.
