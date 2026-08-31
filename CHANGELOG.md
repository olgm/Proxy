# Changelog

## Unreleased

- `proxyd`: config-driven relay node. Handshake rewrite (Forge markers preserved,
  BungeeCord identity stripped), source-IP allowlist, `splice(2)` relay with
  per-direction half-close, legacy `0xFE` ping rejected.
- `proxyctl`: topology-driven deployer over ssh/scp. Derives every node config from
  one file, cross-compiles per node arch, installs a hardened systemd unit running as
  an unprivileged account.
- Verified HK → Tokyo → Chicago → Hypixel: status ping returns Hypixel's MOTD while
  the client claims an unrelated hostname, and all three hops appear in the socket
  table.
