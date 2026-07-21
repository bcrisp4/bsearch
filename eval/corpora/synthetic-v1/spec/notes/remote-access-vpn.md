# Remote access — wireguard notes

Requirement: reach the homelab from anywhere (phone + laptop), expose
nothing else. Chose plain wireguard on the router over the managed mesh
VPN services — one less account in the loop, and the BSc has to be good
for something.

Setup:

- wg listens on the router, port forwarded is n/a (router IS the
  endpoint), UDP 51820 on the Loopfibre static IP
- Static IPv4 (the £5/mo add-on, [[static-ip-and-hosting]]) means no
  dynamic DNS faff — endpoint is just an A record on my domain
- Peers: phone, laptop, plus one for the ThinkPad-in-a-drawer for
  emergencies. Full tunnel OFF for the phone (battery), split tunnel
  routing just 192.168.20.0/24 + the wg subnet
- Keys generated per device, never moved between devices. QR for the
  phone

```
[Peer]
# laptop
PublicKey = <redacted, in the keepass>
AllowedIPs = 10.66.0.2/32
```

Gotchas hit:

- MTU. Symptom: ssh fine, anything bulky hangs. `MTU = 1380` on peers
  fixed it (full fibre + wg overhead, the classic)
- Phone roaming wifi→5G drops for ~25s unless PersistentKeepalive = 25
- The killer app turned out to be the photos service and the notes sync
  feeling local from anywhere. And checking the uptime dashboard from
  the pub, which is either monitoring maturity or a cry for help
