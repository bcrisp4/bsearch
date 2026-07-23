# Networking cheatsheet (the stuff I re-derive otherwise)

Personal crib sheet. Degree was literally in this and I still look up
subnet maths like a fraud. Fine. That's what notes are for.

## Subnets I actually use

| CIDR | Hosts | Used for |
|---|---|---|
| /24 | 254 | main LAN (192.168.20.0/24) |
| /24 | 254 | IoT vlan (192.168.30.0/24) |
| /29 | 6 | wg peers would fit but | 
| /16 | 65k | docker address pool (172.28.0.0/16) |

Quick maths: /27 = 32 addresses = 30 hosts. /28 = 16 = 14. Borrow a bit,
halve the hosts. Yes Theo, again: the network and broadcast addresses
don't count.

## Diagnosis order (the litany)

```
ip a                      # do I have an address
ping <gateway>            # can I reach the router
ping 1.1.1.1              # can I reach the internet by IP
dig @<resolver> example.com   # is it DNS
# it's DNS
```

It was DNS in Brooklyn (twice), it was NOT DNS in January (WAN,
[[broadband-outage-jan-2026]]), the litany doesn't care, run the litany.

## MTU decoder ring

- Symptom "small things work, big things hang" → MTU
- Find path MTU: `ping -M do -s 1472 <host>` and walk down
- wireguard on full fibre: 1380 works, stop fiddling
  ([[remote-access-vpn]])

## Ports burned into memory anyway, listed for completeness

53 dns / 67-68 dhcp / 123 ntp / 443 the world / 51820 wg (mine) /
5335 unbound upstream (mine, [[dns-adblock-setup]])
