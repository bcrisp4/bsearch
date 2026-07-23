---
title: Mini PC server setup
date: 2025-09-21
tags: [homelab, tech, howto]
---

# Mini PC server — build log

The Brooklyn server was an old ThinkPad with the lid closed. The London
upgrade: fanless N100 mini PC, 16GB, 1TB nvme, bought September 2025.
This is the setup log so future me can rebuild it without archaeology.

## Base OS

Debian 12 netinst from USB, ssh server only, no desktop.

```
# after first boot, the ritual
apt update && apt upgrade
apt install sudo vim curl git htop
adduser theo sudo
# ssh: keys only
sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config
systemctl restart ssh
```

Static lease on the router (192.168.20.10) rather than static config on
the box — one place to look, and reinstalls come up with the same IP.

Unattended-upgrades on for security patches, email-on-error pointed at
my normal address via the smarthost config. Boring and correct.

## Containers

Everything in Docker via compose, one directory per service, the whole
tree in a git repo (config only — data dirs are excluded and live under
`/srv/data/<service>`, which is what gets backed up, see
[[backup-strategy]]).

```yaml
# the pattern every service follows
services:
  app:
    image: whatever:pinned-version   # PINNED. learned this the hard way
    restart: unless-stopped
    volumes:
      - /srv/data/app:/data
    networks: [proxy]
```

Reverse proxy owns ports 80/443 and does TLS for everything via the DNS
challenge (domain's DNS provider has an API token scoped to just the
ACME records). Services only ever join the `proxy` network — nothing
else is published on the host. Inbound from the internet is a separate
question ([[static-ip-and-hosting]]) and the answer is mostly "no,
wireguard instead" ([[remote-access-vpn]]).

## Things that bit me

1. **Unpinned images.** The photos app shipped a breaking schema change
   in `:latest` and I got to restore from backup in week 3. Every image
   pinned since, updates are deliberate, one service at a time, after
   reading the release notes like an adult.
2. The nvme ran hot (70°C+) until I lifted the box 2cm off the shelf.
   Passive cooling needs actual airflow, revolutionary insight.
3. Docker's default bridge kept colliding with a subnet I use on
   wireguard — set `default-address-pools` in daemon.json to something
   out of the way (172.28.0.0/16) and the weirdness stopped.
4. Power cut in November: box came back, containers came back,
   *the USB backup disk did not* until re-plugged. Now there's a weekly
   `smartctl` + mount check in the uptime monitor so silent failures
   aren't silent.

## Monitoring the monitor

The uptime service watches everything else, so what watches it? Answer:
a dead-man's-switch style heartbeat — the box pings an external check
service every 5 minutes, and *absence* of the ping alerts my phone. Set
this up after realising the November power cut would have been invisible
if I hadn't been sitting in the room when it happened. The external
check is the only third-party dependency in the whole stack and I have
made my peace with it: self-hosting the "is my house on fire" alarm
inside the house defeats the point, as the January WAN outage
demonstrated with some style.

Logs: journald with a 500MB cap, plus the containers log to local files
rotated weekly. No log shipping, no dashboards beyond the uptime page.
When something breaks I ssh in like it's 2009 and grep. It's one box.

## Deliberately not doing

- Kubernetes. It's one box. I do enough YAML at work
- VMs/hypervisor — containers cover everything I actually run
- Public-facing services beyond the two behind the proxy. Attack surface
  small, sleep quality high

Total cost: ~£180 for the box, runs at 8-14W. The old ThinkPad is
honourably retired to the crash-cart drawer ([[homelab-inventory]]).
