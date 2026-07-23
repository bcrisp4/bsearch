---
title: Kestrel solver incident — retro notes
date: 2026-02-12
tags: [work, halewick, kestrel, incident]
---

# Retro: the great re-optimisation stampede (INC-2026-014)

My personal notes from the retro, 12 Feb. Official doc is on the wiki;
this is the version with opinions.

## What happened

Tuesday 3 Feb, 07:40-09:55. A malformed depot record (capacity curve
with a zero-length window — yes, my feature) made Kestrel's solver throw
on one customer's morning plan. The retry queue did what retry queues do:
every retry re-enqueued a full re-optimisation, the solver pool
saturated, and *other* customers' morning runs queued behind the
poison job. ~40 fleets got their routes 30-90 min late. Peak morning
dispatch window, of course. It's always the peak window.

## Timeline (abridged)

```
07:40 first solver panic, job requeued
07:52 solver pool at 100%, queue depth alarm fires
08:05 on-call (not me, mercifully) pages platform
08:31 poison job identified, quarantined by hand
08:47 queue draining, but thundering-herd of stale re-opts
09:55 all fleets re-planned, incident closed
```

## What I took from it

1. **My validation gap.** The API accepted a zero-length window because I
   validated start < end at the *day* level but not per-segment after
   midnight wrap. Fix shipped 4 Feb. Test added. Shame retained.
2. Poison-job quarantine was manual — 26 minutes of a senior engineer
   eyeballing job payloads. We're adding an auto-quarantine after N
   solver panics on the same job hash. I volunteered to build it, partly
   penance, partly it's genuinely interesting.
3. Retry with backoff is not enough when the job itself is the bomb.
   Everyone nodded. Everyone has the scar tissue now.
4. Blameless retro culture here is real. Dev opened with "the system let
   a bad record in, Theo just wrote the code the system accepted", which
   — having been at places where that sentence goes differently — I
   appreciated a lot.

## Actions on me

- [x] Per-segment window validation (shipped, 4 Feb)
- [x] Property-based tests for capacity curve parsing
- [ ] Auto-quarantine design note by end Feb
- [ ] Add the zero-length-window case to the onboarding "cursed inputs"
  doc, which I am now a contributor to. Growth.
