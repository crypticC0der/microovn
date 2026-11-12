# `microovn setup bgp-attach`

Attach mode currently needs six markers to be set: two in every chassis OVS
and four in OVN NB.

On each node:

```bash
sudo ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp=enabled
sudo ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp-advertise-table=42
```

Cluster wide:

```bash
LRP=bgp-lrp-bgp-lr-stg-reproducer-hassahma-compute-0-to-bgp-ls-stg-reproducer-hassahma-compute-0-br-bgp

sudo ovn-nbctl set logical_router_port $LRP \
  networks="192.0.2.3/31" \
  options:dynamic-routing-maintain-vrf=true \
  external-ids:microovn-bgp-attach=true \
  external-ids:microovn-bgp-peers="65101@192.0.2.2"
```

This PR adds commands that configure it per leaf (rack), so Sunbeam can
drive it. Each compute is labelled with its rack (the MicroOVN charm does
this from the role-distributor `rack-name` workload param):

```bash
microovn setup bgp-attach label --rack rack1
```

Then the rack's peering is set once, on any compute:

```bash
microovn setup bgp-attach --peer 65101@192.0.2.17 --networks 192.0.2.18/28 --rack rack1
```

Here, `192.0.2.18/28` is the range for assigning IPs to the computes labelled
`rack1`, starting from `192.0.2.18`. With c0, c1 and c2 labelled `rack1`:

```
192.0.2.18 -> c0
192.0.2.19 -> c1
192.0.2.20 -> c2
```

MicroOVN owns the lifecycle of creating the veth interface on each node and
assigning an IP address to it:

```
mp977ed87fo@mp977ed87fb  UP  ...
mp977ed87fb@mp977ed87fo  UP  192.0.2.18
```

## Racks and membership

Run the command once per rack. Each `--peer` is a separate setting with its
own `--networks` and `--rack`.

```bash
# rack1 computes peer with leaf 1
microovn setup bgp-attach --peer 65101@192.0.2.17 --networks 192.0.2.18/28 --rack rack1

# rack2 computes peer with leaf 2
microovn setup bgp-attach --peer 65102@192.0.2.33 --networks 192.0.2.34/28 --rack rack2
```

- Members follow their labels, with no rerun: labelling a compute adds it,
  changing its label moves it to the other rack in one step, and
  `label --clear` (or removing it from the cluster) removes it and frees its IP.
- A compute that stays keeps its IP.
- A label only changes when it is set or cleared; a late label never removes
  anyone.
- One setting per rack, and each rack needs its own subnet; overlapping
  `--networks` or a shared leaf address are refused.
- `microovn setup bgp-attach status` lists every rack, peer, compute and IP;
  `microovn setup bgp-attach disable --peer <peer>` removes one rack's setting
  (labels stay).
