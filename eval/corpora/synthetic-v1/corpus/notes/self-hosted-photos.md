# Self-hosted photos — migration notes

Oct 2025. Goal: out of the big-cloud photo silo before the next storage
tier upsell, without losing ML search or phone auto-upload, i.e. the
features that made the silo sticky.

Done:

- Photos service running containerised on the mini PC, library on the
  nvme, under `/srv/data` so it rides the backup chain automatically
  ([[backup-strategy]])
- Takeout-style export from the old provider: 94GB, arrived as 47 zip
  files with metadata in sidecar JSONs because of course it did. Used
  the community metadata-merge tool, spot-checked ~50 photos, dates and
  GPS survived. The 2 weeks of Brooklyn photos with no EXIF are a 2019
  phone's fault, not the migration's
- Phone app auto-uploads on wifi; when out, it queues. With wireguard up
  it can also sync remotely but battery says use it sparingly
- Face/object recognition ran overnight x3 nights on the N100. Slower
  than the cloud, obviously, and the "search: sofa" test finds the
  Calder & Roe delivery day photos, so good enough

Kept the old account at the free tier as a lurking fallback for one more
year. Belt, braces.

Sharing: public share links go through the reverse proxy on the static
IP ([[static-ip-and-hosting]]) with expiry set. Sent Mum a link, she
opened it, nobody had to install anything. The bar for family tech is
underground and I cleared it.
