---
title: Backup strategy
date: 2026-01-11
tags: [homelab, tech, backups]
---

# Backups — the 3-2-1-ish setup

Written down properly in January 2026 after the second "wait, IS that
backed up?" conversation with myself. The answer must never again be a
shrug.

## What gets backed up

Tier 1 (irreplaceable): photos, documents scan folder, the notes vault
(this thing), keepass db. ~180GB, mostly photos.

Tier 2 (annoying to lose): service configs + databases from the mini PC
(`/srv/data`), the compose git repo — though that's also pushed to a
remote anyway.

Tier 3 (re-downloadable): media. NOT backed up, it's a cache with
ambitions. Losing it costs a weekend, not a memory.

## How

restic, three targets:

1. **Local**: the 4TB USB disk on the mini PC. Nightly at 02:00, cheap
   and fast to restore from
2. **Offsite**: cloud object storage (the cheap S3-compatible kind),
   nightly after the local run. Encrypted client-side by restic so the
   provider sees noise
3. **Cold-ish**: a 2TB portable drive that lives at Mum's in Ottersley,
   refreshed when I visit, so roughly quarterly. Ransomware/fire/burglary
   insurance. Last refreshed: Easter

```
# the shape of it (real scripts in the repo)
restic -r /mnt/backup/restic backup /srv/data /home/theo/vault \
  --exclude-file=/etc/restic/excludes.txt
restic -r /mnt/backup/restic forget --keep-daily 7 --keep-weekly 5 \
  --keep-monthly 12 --prune
```

Phone photos auto-upload to the photos service on wifi
([[self-hosted-photos]]), which puts them under `/srv/data`, which puts
them in all three targets. The chain matters: phone loss = zero photo
loss as long as I've been home since.

## Verification (the bit everyone skips)

- `restic check` weekly, cron, result lands in the uptime monitor
- **Actual restore test**: first Sunday of the quarter, restore a random
  directory from the *offsite* repo to /tmp and diff it. Takes 10
  minutes. Done Jan 4 ✓, Apr 5 ✓ (found the excludes file was skipping
  the keepass db backup copy — fixed, and this is exactly why you test)
- The November power cut taught me the USB disk can silently not come
  back ([[mini-pc-server-setup]]) — mount check now alarms

## The threat model, honestly stated

Wrote this out because "backups" without a threat model is just vibes:

1. **Disk death** — covered by target 1, restore in an evening
2. **My own fat fingers** (`rm` the wrong thing, bad script, the photos
   app schema incident) — covered by snapshots + the retention policy;
   the 7 daily keeps have already paid out once
3. **Burglary/fire/flood at the flat** — covered by offsite + the
   Ottersley drive. This is the scenario that actually scares me: the
   mini PC and the USB disk are 40cm apart on the same shelf
4. **Ransomware/account compromise** — restic's append-mostly setup
   plus a separate credential for the offsite bucket that can't delete
   old snapshots. The Ottersley drive is the true cold fallback: offline
   is a security feature no cloud can match
5. **Cloud provider dies/locks me out** — two other copies, shrug

Not covered and consciously so: simultaneous destruction of London AND
Hampshire (at which point photos are not my biggest problem), and
anything requiring me to remember a password I haven't written into the
keepass — which is itself in tier 1, in all three targets, plus a
printed emergency sheet in the fireproof pouch. Turtles most of the way
down, but the bottom turtle is paper.

## Known gaps, accepted or not

- [ ] Laptop itself only backs up when docked at the desk — flaky habit,
  make it a login hook instead
- [x] ~~Email~~ — mailbox export quarterly into the documents folder, done
  alongside the drive refresh
- Mum's-house drive is unencrypted-at-rest → it is now (Jan), fixed while
  writing this note. Writing things down works
- Restore time for full photo library from offsite: ~9 hours at current
  line speed. Acceptable. The £5 static IP does not make uploads faster,
  sadly ([[static-ip-and-hosting]])
