---
title: Halewick onboarding notes
date: 2025-08-15
tags: [work, halewick]
---

# Halewick — first two weeks

Started 11 Aug. Notes to self while everything is still new enough to
notice.

The company in one paragraph: Halewick sells dispatch and routing
software to mid-size courier and field-service fleets. Two products that
matter — **Waybill** (the API + dashboard customers integrate with) and
**Kestrel**, the route optimisation engine underneath, which is where I
sit. Platform team is 6 people. Everything runs on the usual cloud
suspects, Go services, Postgres, too much YAML. Felt at home by day 3.

People:

- Manager: Dev, runs platform. Weekly 1:1 Thursdays (log kept separately)
- Onboarding buddy: Priti — knows where every skeleton is buried and
  which dashboards lie
- HR is Elena Vasquez-Reid, sorted my payroll ref (HAL/2214) and the
  pension paperwork same day I asked. P60s and payslips land in the
  portal, remember to download them, they expire off it after 2 years
  apparently??

First-fortnight gotchas for future reference:

- The staging environment is named `prod-mirror` which is terrifying
  every single time
- Kestrel's test suite takes 40 min; nobody runs it locally, CI or death
- Payroll cutoff is the 20th — started on the 11th so August was a
  part-month payslip, September is the first normal one (and the first
  with the student loan deduction, ugh)

Goal for probation (6 months, so mid-Feb): own something end-to-end. Dev
suggested the depot-capacity feature. Taking it.
