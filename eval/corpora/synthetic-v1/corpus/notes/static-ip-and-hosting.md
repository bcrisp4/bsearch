---
title: Static IP and what actually gets exposed
tags: [homelab, networking]
---

# The £5 static IP — worth it?

Loopfibre do a static IPv4 add-on for £5/mo on top of the £29.99 Loop
Fibre 150 (well, £32.99 since May). Took it at install time (Aug 2025).
Periodic justification review, because £60/yr:

**What it's for:**

- wireguard endpoint as a stable A record — no dynamic DNS updater to
  break silently ([[remote-access-vpn]])
- Inbound 443 to the reverse proxy for exactly two things: the photos
  share links ([[self-hosted-photos]]) and the status page
- Clean SMTP-adjacent reputation not an issue since I don't self-host
  mail (see rule below)

**Rules of engagement** (written after the January incident sharpened
the mind, [[broadband-outage-jan-2026]]):

1. Nothing listens on the WAN except UDP 51820 and TCP 443
2. 443 terminates at the reverse proxy; anything sensitive is
   wireguard-only regardless
3. No self-hosted email, ever. I know how that story ends and it ends
   with a weekend gone and mail in spam anyway
4. Router does the firewalling; the mini PC assumes the LAN is hostile
   anyway (keys-only ssh, services bound to the proxy network)

Verdict: keeping it. The dynamic-DNS-breaks-while-abroad failure mode
alone is worth £5/mo. Revisit if Loopfibre ever do proper static IPv6
prefix delegation instead — then the v4 becomes vestigial.
