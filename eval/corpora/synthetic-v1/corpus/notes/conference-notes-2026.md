---
title: SysNet London 2026 — conference notes
date: 2026-04-24
tags: [work, conference, networking]
---

# SysNet London 2026 (23-24 April)

Work paid (invoice went through expenses — badge + two days was a few
hundred quid, worth every penny for once). First conference since moving
back. Notes by talk, keeping only the ones I'd actually reference again.

## Day 1

**Keynote — "The boring decade of infrastructure"**
Thesis: the interesting work now is reliability economics, not novel
architecture. Half the room nodded, half bristled. I nodded. The bit
worth keeping: his "cost of a nine" framework — each additional nine
should have a named beneficiary or you're buying jewellery, not
reliability. Stealing this for the Kestrel SLO conversation.

**Queueing theory for people who ship**
Best talk of the day. Little's law applied to incident retry storms —
uncomfortably relevant to the February incident. Key takeaway:
bounded retry *budgets* per tenant, not per job. Presenter's rule of
thumb: if your retry queue can grow faster than your drain rate for
longer than your patience, you've built a bomb with a timer you can't
see. Sent the slides link to Dev before the talk even ended.

**eBPF observability without the hero worship**
Good practical content on overhead measurement. Demo gods took their
sacrifice (kernel version mismatch, live on stage, brutal). Main note:
their flame-graph diffing workflow would work for Kestrel solver
profiling.

## Day 2

**Panel: on-call that doesn't eat people**
The usual, but one gem — a company that pays a flat "disruption fee" per
page regardless of severity, which made teams actually fix noisy alerts
because the cost showed up in a budget. Incentives!

**IPv6 in brownfield networks**
Went for nostalgia (final-year project flashbacks — Wexcombe, 2015, dual
stack lab, the pain is eternal). Left early, it was a vendor pitch in a
trench coat.

**Postgres at uncomfortable sizes**
Wandered in because the queueing room was full, stayed because the
speaker was excellent. Practical partitioning war stories, and one line
I wrote down verbatim: "your database is fine, your access patterns are
a crime scene". Kestrel's route-history table is approaching the sizes
under discussion, forwarded the partitioning section to Priti with a
"this is us in ~18 months" note.

**Lightning talks**
Mixed bag as ever. Two keepers: a 5-minute demo of chaos-testing DNS
failure specifically (everyone tests service death, nobody tests
slow-DNS, and slow beats dead for damage every time — true at work AND
at home, the January outage taught the home version); and someone's
tooling for diffing firewall rulesets across environments, which I want
for exactly one annual homelab audit and will therefore never install.

**Hallway track**
- Met two people from a courier competitor. Carefully said nothing, drank
  the coffee, learned they also have a depot-capacity problem. Everyone
  does. Validating.
- Someone demoed self-hosted status pages on the sponsor floor and now I
  have homelab ideas again, which is a warning sign

## Meta-notes on conference technique (first one in years)

- Skipping a slot to digest notes in the corridor beat attending a
  fourth mediocre talk, every time. Schedule the gaps on purpose
- The sponsor floor coffee is a trap in both senses; the good coffee
  was the cart outside the north entrance
- Badge says "Halewick Systems" and three separate people opened with
  "oh, the routing people?" — apparently we have a reputation, a nice
  thing to learn at neutral venues
- Should have brought business cards. Wrote my email on four napkins
  like it's 1997

## Follow-ups

- [x] Slides to Dev (queueing talk)
- [ ] Write up retry-budget idea as a proposal — pairs with the
  auto-quarantine work
- [ ] Expense the badge invoice before finance's month-end cutoff
