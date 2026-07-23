# Template conventions — synthetic-v1

Read before authoring any template. The exemplar is
`bills/energy_bill.html` + `../data/energy_bills.yaml` — copy its patterns.

## Contract

- Templates are Jinja2 HTML rendered by WeasyPrint (A4 default; add
  `page: us-letter` styles for US docs). Every template links
  `../base.css` and uses its component classes (letterhead, addr-window,
  meta-box, total-banner, summary, table.data, panel, rail, small-print,
  page-footer). Template-specific styles go in a `<style>` block.
- Fonts: only the vendored Liberation families via base.css. Never name a
  system font.
- Context = the whole persona pack (keys `person`, `addresses`, `banks`,
  `vendors`, `employers`, `supporting_cast`, `timeline`) plus `vendor`
  (the resolved vendor entry — only when the data entry names a
  `vendor_key`; entries with `vendor_key: null` carry their own fields and
  their templates never reference `vendor`) and `issue` (one entry from
  the data file). Jinja runs StrictUndefined: referencing a missing key
  fails the build — on purpose. For *optional* issue keys use
  `{% if issue.x is defined %}` or `{{ issue.x | default("...") }}` —
  a bare `{% if issue.x %}` raises under StrictUndefined.
- **Every personal value comes from persona.yaml via the context.** A
  literal name, address, account number, or personal date in a template or
  data file is a bug. Invented incidental values (meter serials, clerk
  initials, invoice line items) live in the data file, not the template.

## Data files (`../data/*.yaml`)

- One file per document set (matching `corpus.yaml`): `template`,
  `vendor_key`, and `issues:` (series) or `entries:` (singles). Every
  issue/entry has a stable `id` — it becomes the output filename.
- Series entries MUST differ in prose, not just numbers (spec rule): use
  the rotating notice/panel slot. At least the slots named in
  `corpus.yaml` must appear across the series.

## Realism rules

- **Arithmetic adds up.** Totals = sum of parts; running balances carry
  issue to issue; VAT is computed, not invented. Real documents balance —
  broken sums are a tell and poison amount-targeting golden queries.
- Dates respect `timeline` in persona.yaml (US docs only in the US era).
- UK/US furniture per the shape notes (`~/bsearch-eval/shape-notes/*.md`,
  local-only): UK = sort codes, FSCS/ombudsman-style footers, accessibility
  offers, A4; US = form codes, federal notices, PO boxes, Letter size.
- Registers vary within documents (brand voice + regulatory small print).

## Safe fake ranges

- UK phones: `020 7946 0xxx` / `0113 496 0xxx` / mobile `07700 900xxx`
  (Ofcom drama ranges). US phones: `xxx 555-01xx`.
- Domains/emails: fictional name + `.example` TLD.
- Never name a real company, charity, public body, or scheme brand beyond
  public *format* names (W-2, SA100, EPC…). Regulators may be referenced
  generically ("the statutory ombudsman"), not by proper name — several
  real public-body names are denylisted and will fail the gate.

## Verification (every batch, before reporting done)

```sh
cd /Users/ben/src/bsearch/eval/generate
uv run corpusgen check --denylist ~/bsearch-eval/denylist.txt /Users/ben/src/bsearch/eval
# and render-test each new template (see exemplar snippet in eval/README.md)
DYLD_FALLBACK_LIBRARY_PATH=/opt/homebrew/lib uv run python <render-test>
```
