# Security

## Reporting a vulnerability

Please report security issues through GitHub's private vulnerability
reporting on this repository (the "Security" tab, "Report a vulnerability"),
or by opening a security advisory. Do not open a public issue for a
vulnerability. Do not email us; we don't have an address to give you for
this.

## Scope

- `proxyd`'s TCP listeners (Minecraft ingress and relay hops)
- the UDP tunnel between nodes
- the control link (the keyed link proxyctl and the bot use to manage a
  whitelist)
- the Discord bot (`proxybot`)

## Threat model

The whitelist is a door lock, not authentication: it gates on what a client
claims in its Login Start packet, which is unverifiable and always will be.
Do not report "the whitelist can be bypassed by lying about identity" as a
vulnerability; that is the known shape of the mechanism.

UDP datagrams are authenticated per leg by an AEAD key, never by source
address. A source address is forgeable over UDP, so nothing in the tunnel
trusts one. A finding that a UDP peer can be spoofed by address alone, in a
context where the key check is bypassed, is in scope.

Secrets (tunnel keys, control keys, the Discord bot token, webhook URLs) live
only in env files and config on the nodes that need them, and are never
committed to this repository. A Discord bot token or webhook URL is a bearer
credential: anyone holding it can act as the bot or post to the channel
without further proof of identity.
