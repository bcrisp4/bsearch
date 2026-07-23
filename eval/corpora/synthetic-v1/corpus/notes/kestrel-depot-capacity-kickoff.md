---
title: Kestrel depot capacity — kickoff meeting
date: 2025-10-02
tags: [work, halewick, kestrel, meetings]
---

# Depot capacity constraints — kickoff, 2 Oct 2025

Attendees: me, Dev, Priti, Marcus from product, two folks from the
customer success side dialled in.

## Problem

Kestrel currently treats depots as infinite: it'll happily route 400
vans through a depot that can physically load 120/hour. Customers
hand-fix this with fake "closed" windows, which breaks re-optimisation
mid-day. Second most-requested thing on the Waybill feedback board.

## Shape of the fix (agreed)

- New per-depot `capacity` model: loading bays × throughput/hour, with
  optional time-of-day curve
- Kestrel treats capacity as a soft constraint with configurable
  penalty, NOT hard — hard constraints made the solver fall over on the
  synthetic tests when demand > capacity (which is exactly when
  customers need answers most)
- Waybill API: new fields on the depot resource, feature-flagged,
  backwards compatible. Old integrations see no change

## Decisions / actions

- [x] Me: design doc for the capacity model by 17 Oct
- [x] Priti: pull real depot throughput data from the two pilot
  customers so the defaults aren't fiction
- [ ] Marcus: pricing question — is this in the base tier or the
  "advanced constraints" add-on? (still unresolved 3 meetings later, lol)
- [x] Spike: does the penalty approach blow up solve times? Budget: 1 wk

Parking lot: multi-depot load balancing (do NOT let this creep in),
driver-break rules interacting with depot queues (v2).
