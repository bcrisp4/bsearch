---
title: Homelab inventory
tags: [homelab, tech]
---

# Homelab — what's running (updated Jun 2026)

Small on purpose. The rule since Brooklyn: every box must earn its watts.

| Device | Role | Notes |
|---|---|---|
| Mini PC (N100, 16GB, 1TB nvme) | the server | Debian 12, everything in containers — [[mini-pc-server-setup]] |
| Pi 4 (2GB) | DNS + dhcp | blocklist resolver, see [[dns-adblock-setup]] |
| Loopfibre hub | modem/router | in bridge-ish mode, wifi off |
| Own router (used, eBay) | routing + wifi + wireguard | the actual network brain |
| 4TB USB HDD | backup target | plus offsite — [[backup-strategy]] |
| Old ThinkPad | crash cart / spare | drawer until needed |

Services on the mini PC (compose files in the git repo):

- photos ([[self-hosted-photos]])
- media server
- wiki/notes sync
- uptime monitor + status page (the conference sponsor floor got me,
  see [[conference-notes-2026]], I regret nothing)
- reverse proxy in front of everything, TLS via DNS challenge

Idle draw for the lot: ~14W measured at the plug. Earning their watts.

Network: 192.168.20.0/24 main, 192.168.30.0/24 for the sketchy IoT
stuff (the smart plugs never talk to the internet again). Static IPv4
from Loopfibre (£5/mo) does the heavy lifting for inbound —
[[static-ip-and-hosting]].
