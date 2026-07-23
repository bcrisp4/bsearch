---
title: Broadband outage, Jan 2026
date: 2026-02-03
tags: [homelab, broadband, admin]
---

# The January Loopfibre outage — log + the £65 sting

**Sun 18 Jan, ~16:20** — internet gone. Hub light flashing red. Local
network fine (thank you [[dns-adblock-setup]] fallback design — instantly
obvious it was WAN-side, not my stack):

```
$ ping -c3 192.168.20.1     # router: fine
$ ping -c3 hub              # hub responds
$ ping -c3 1.1.1.1          # nothing. WAN dead
```

Loopfibre status page (checked via phone tethering): no area fault. So
it's just me. Reported the fault same evening via the app.

**Wed 21 Jan** — engineer visit, morning slot, actually on time. Traced
it to a damaged internal socket — the faceplate in the hallway, which,
being inside the flat, is chargeable territory. He was upfront about it
before doing the work, replaced the faceplate, connection back in 20
minutes. Speeds fine since.

**Feb bill** — there it is: "Engineer visit — fault repair, £65.00",
taking the month to £99.99. The bill even has a little notice panel
explaining the charge with the dates. Grumbled, checked the T&Cs,
internal wiring is genuinely on me (tenant, not even Wrenmoor's problem
since it wasn't the line to the property). Paid. Filed under "renting:
misc damage, origin unknown, invoice: mine".

Post-mortem for the homelab habit tracker:

- 3 days offline, and the wireguard/photos/notes stack was obviously
  unreachable from outside the whole time. Tethering covered the basics
- The uptime monitor dutifully recorded the outage it could not alert me
  about, because it alerts... over the internet. Noted the irony. Added
  the status service's external check as the canary instead
- Phone tethering data cap survived, barely. WFH Tuesday from the office
  instead, which Dev found funnier than necessary
