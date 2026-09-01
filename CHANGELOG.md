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
- `tools/mcping`: status-ping client for checking a chain end to end. Outside the
  deploy path; build and copy it by hand.
- Provisioned `tyo-01`, a second Tokyo node (ty-a `nrt`, `vc2-1c-1gb`, Ubuntu 26.04,
  auto-backup off) at 198.51.100.21. Not referenced by `topology.json` yet. Node-to-node
  ICMP baseline, 20 echoes each, 0% loss: HK 44.6 ms avg, Chicago 136.1 ms avg.
- Verified HK → Tokyo → Chicago → Hypixel: status ping returns Hypixel's MOTD while
  the client claims an unrelated hostname, and all three hops appear in the socket
  table. Real client login through the chain confirmed working.
