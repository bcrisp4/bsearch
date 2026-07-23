---
title: DNS + adblock on the Pi
tags: [homelab, tech, howto]
---

# Blocklist DNS on the Pi 4

The Pi runs the resolver for the whole flat: blocklist filtering + local
names, with unbound behind it doing actual recursive resolution (no
forwarding to the ISP or a big public resolver — the whole point is
fewer parties seeing lookups).

Flow: clients → filtering resolver (port 53) → unbound (127.0.0.1#5335)
→ the internet.

unbound bits that matter:

```
server:
  interface: 127.0.0.1
  port: 5335
  hide-identity: yes
  hide-version: yes
  prefetch: yes
  cache-min-ttl: 300
```

Local names via a local zone on the filtering layer: `pi.lan`,
`server.lan` (the mini PC), `router.lan`. DHCP hands out the Pi as DNS.

Ops notes:

- Blocklists update weekly, Sunday 4am. One list once nuked a supermarket
  click-and-collect flow; whitelisted, moved on. Aggressive lists aren't
  worth the domestic support tickets
- The router hands out the Pi's address via DHCP — but the IoT vlan
  (192.168.30.x) gets NO dns at all beyond the local zone, those devices
  phone home enough as it is
- Failure mode: Pi dies → internet "down" for the flat. Happened once
  (SD card, obviously — since moved the Pi to network boot off the mini
  PC). Secondary DNS in DHCP now points at the mini PC running a plain
  resolver as fallback, degraded but alive
- When the Loopfibre outage hit in January ([[broadband-outage-jan-2026]])
  everything LOCAL kept resolving, which made diagnosing it faster —
  could tell instantly it was the WAN, not my stack, before ringing them
