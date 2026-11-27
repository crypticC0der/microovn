=============================
Configure OVN BGP integration
=============================

Configuration of the OVN integration with BGP is a single-command process in
the MicroOVN, for more information about what's happening under the hood, see:
:doc:`Explanation: OVN integration with BGP </explanation/bgp-redirect>`.

.. note::

   This is experimental and the network architecture this sets up is likely to
   change in future versions. This use case will not be the long term solution,
   and nor will deployments using MicroOVN BGP in its current state be fully
   supported going forward.


Run BIRD standalone
-------------------

The BIRD routing daemon that MicroOVN uses for BGP is shipped as a standalone
snap service, ``microovn.bird``, so it can be run and configured without the rest
of MicroOVN's BGP machinery — the same way ``microovn.switch`` runs Open vSwitch
on its own.

.. code-block:: none

   sudo snap start microovn.bird

BIRD reads its configuration from ``/var/snap/microovn/common/data/bird/bird.conf``
and is controlled with the ``microovn.birdc`` client:

.. code-block:: none

   microovn.birdc show status
   microovn.birdc configure

This is the first step of the BGP integration ladder: run BIRD on its own, then
set the configuration keys described below, and finally enable the attach
capabilities that let MicroOVN drive BIRD on your behalf.

.. important::

   When BGP is enabled (managed or attach mode), MicroOVN takes over the BIRD
   configuration file and overwrites it. Running a standalone BIRD *and*
   MicroOVN-managed BGP on the same host is not supported in v1.


Enable BGP integration
----------------------

In this example, we have a host connected to two external networks via
interfaces ``eth1`` and ``eth2``.

The only required configuration is specifying the external connection interfaces.
Both `VRF`_ table number and AS number are optional:

- If VRF is not specified, MicroOVN will automatically select an available VRF table ID
- If AS number is not specified, MicroOVN will automatically generate an unique ASN

.. important::

   Never use interface that provides actual host connectivity for the purpose
   of OVN BGP integration. These interfaces are meant for the OVN's traffic,
   they will be assigned to a OVS bridge and you will lose your connection to the host.

Basic usage with automatic configuration
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

To enable BGP integration with automatic VRF and ASN selection:

.. code-block:: none

   microovn enable bgp --config ext_connection=eth1,eth2

MicroOVN will automatically:

- Select an available VRF table ID
- Generate a unique ASN from the default range (4210000000-4294967294)

.. note::

   The default ASN range reserves the first IDs of the full `RFC 6996`_ private range (4200000000-4209999999)
   for lower tier network infrastructure components (switches, routers, top-of-rack equipment).

Advanced configuration
~~~~~~~~~~~~~~~~~~~~~~

You can explicitly specify VRF table ID, ASN, or provide a custom ASN range, in any combination.

Specify both VRF and ASN explicitly:

.. code-block:: none

   microovn enable bgp --config ext_connection=eth1,eth2 --config vrf=10 --config asn=4210000000

Provide a custom ASN range for automatic selection:

.. code-block:: none

   microovn enable bgp --config ext_connection=eth1,eth2 --config asn_range=4210000000-4210999999

.. important::

   When using a custom ASN range, provide one sufficiently large to avoid collisions between cluster members.
   Take into account that the ASN that would be allocated for a certain cluster member will never be reused by
   another, even if the first one is removed from the cluster.

You will receive positive confirmation message in the CLI and the setup is
done.

Inspect the changes
~~~~~~~~~~~~~~~~~~~

If you are interested in changes that the MicroOVN made to the system, We can
inspect the new interfaces that were created.

.. code-block:: none

   ip link

The output will show that we have a new VRF device and two veth pairs for
the BGP control-plane traffic (one pair for each external interface):

.. code-block:: none

   <snipped preceding output>
   13: ovnvrf10: <NOARP,MASTER,UP,LOWER_UP> mtu 65575 qdisc noqueue state UP mode DEFAULT group default qlen 1000
       link/ether 2e:25:b7:f0:f7:21 brd ff:ff:ff:ff:ff:ff
   14: veth1-brg@veth1-bgp: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue master ovs-system state UP mode DEFAULT group default qlen 1000
       link/ether 46:d7:9e:a5:81:fa brd ff:ff:ff:ff:ff:ff
   15: veth1-bgp@veth1-brg: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue master ovnvrf10 state UP mode DEFAULT group default qlen 1000
       link/ether 02:0e:da:a6:c4:28 brd ff:ff:ff:ff:ff:ff
   16: veth2-brg@veth2-bgp: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue master ovs-system state UP mode DEFAULT group default qlen 1000
       link/ether ba:1e:35:32:8b:36 brd ff:ff:ff:ff:ff:ff
   17: veth2-bgp@veth2-brg: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue master ovnvrf10 state UP mode DEFAULT group default qlen 1000
       link/ether 02:66:5b:b1:78:6e brd ff:ff:ff:ff:ff:ff
   <snipped remaining output>

And since we requested auto-configuration of BGP daemon, we can check the
BIRD configuration found at ``/var/snap/microovn/common/data/bird/bird.conf``.
There should be two "bgp" instances instances.

.. code-block:: none

   <snipped preceding output>
   protocol bgp microovn_eth2 {
       router id 192.0.2.10;
	    interface "veth2-bgp";
	    vrf "ovnvrf10";
	    local as 4210000000;
	    neighbor range fe80::/10 external;
	    dynamic name "dyn_microovn_eth2_";
   <snipped remaining output>

and

.. code-block:: none

   <snipped preceding output>
   protocol bgp microovn_eth1 {
       router id 192.0.2.10;
	    interface "veth1-bgp";
	    vrf "ovnvrf10";
	    local as 4210000000;
	    neighbor range fe80::/10 external;
	    dynamic name "dyn_microovn_eth1_";
   <snipped remaining output>

.. note::

   Note that for then neighbour configuration, we are not using the names of
   actual physical interfaces (e.g. ``eth1``), but the names of the interfaces
   that were created for BGP redirect (e.g. ``eth1-bgp``)

If there are BGP neighbours already running and configured on the external
networks, you can validate that they successfully established connections:

.. code-block:: none

   microovn.birdc show protocols

The output should contain established BGP sessions.

.. code-block:: none

   <snipped preceding output>
   microovn_eth1 BGP        ---        start  15:21:14.086  Passive
   microovn_eth2 BGP        ---        start  15:21:14.086  Passive
   dyn_microovn_eth1_1 BGP        ---        up     15:37:34.578  Established
   dyn_microovn_eth2_1 BGP        ---        up     15:38:00.689  Established
   <snipped remaining output>

.. _manual_bgp:

Manual BGP daemon configuration
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

In case that the automatic BIRD configuration provided by MicroOVN does not
suit your needs, you can just use the ``--manual-bgpd-config`` flag when
enabling BGP, for example:

.. code-block:: none

   microovn enable bgp --config ext_connection=eth1,eth2 --manual-bgpd-config

With this flag, MicroOVN won't configure the built-in
BIRD daemon, Allowing you to perform manual configuration or use entirely
different BGP daemon.

Attach mode (externally owned routers)
--------------------------------------

When the deployment already has a component that owns the OVN topology —
OpenStack Neutron, Sunbeam or MicroCloud — MicroOVN's regular BGP setup would
collide with it, because it creates its own logical routers and switches.
Instead, MicroOVN can *attach* BIRD to an existing logical router port that the
topology owner created, via OVN's native ``options:routing-protocol-redirect``,
without owning any L2/L3 topology of its own.

Attach mode is driven by per-port intent, with no chassis-wide enable gate:
MicroOVN runs attach because a router pinned to the local chassis carries the
attach marker.

- ``microovn-bgp-attach=true`` on a ``Logical_Router_Port`` marks that port as an
  attachment point (the mode marker).
- ``microovn-bgp-peers=<asn>@<address>[, ...]`` on the same port lists the eBGP
  peer(s) — the only per-node value the owner supplies.

MicroOVN records which ports it has attached to in a single local
``Open_vSwitch`` external-id: ``microovn-bgp-attached`` holds a comma-separated
list of logical-router-port names. This journal lives in MicroOVN's own OVS DB
(not the NB), so it survives a topology owner rewriting NB ``external_ids`` and
is inherently chassis-scoped.

The chassis-wide knobs live on ``Open_vSwitch`` ``external_ids``:

- ``microovn-bgp-asn`` — local ASN (optional; auto-generated from the cluster
  member ID when absent).
- ``microovn-bgp-router-id`` — BIRD router id (optional; auto-generated).
- ``microovn-bgp-advertise-table`` — the kernel table the topology owner /
  BGP producer populates with chassis-local ``/32`` host routes (with a kernel
  metric) that BIRD advertises out. Optional: it defaults to the learn-table
  (the VRF MicroOVN allocated), which is where Neutron's native
  ``dynamic-routing-redistribute=nat`` writes FIP ``/32``\ s. Set a distinct
  value only to use a separate producer table.
- ``microovn-bgp-import-filter`` — which routes BIRD accepts *from* attach
  peers (optional). One of:

  - ``default`` (or unset) — import only the default route ``0.0.0.0/0``. This
    is the classic spine-leaf contract: a leaf compute only needs a default
    from its ToR.
  - ``all`` — import every route the peer sends.
  - a comma-separated list of IPv4 prefixes — import the default route plus
    those prefixes (e.g. ``172.0.0.0/8,10.149.0.0/16``), for fabrics that
    advertise specific segments rather than just a default. Each prefix is
    matched inclusively (the prefix and all of its subnets), so ``172.0.0.0/8``
    imports ``172.20.0.0/24`` and every other more-specific route within it.
    The default route is always imported, so listing ``0.0.0.0/0`` is rejected.
    Up to 32 prefixes may be listed.

  The imported routes are installed into the OVN VRF (``learn-table``) the same
  way the default route is.

Per-port configuration lives on the tagged ``Logical_Router_Port``
``external_ids``:

- ``microovn-bgp-peers`` — comma-separated ``<asn>@<address>`` eBGP peers. The
  address family and the peer's subnet decide the peering mode:

  - **unnumbered** (IPv6 link-local peers, e.g. ``65002@fe80::1``): the LRP
    sends periodic RAs so the attachment veth acquires the LRP's link-local
    address;
  - **numbered** (an IPv4 peer): the IPv4 address must fall inside one of the
    LRP's ``networks`` — that network is the transit prefix, and the LRP's own
    address in it becomes the local BGP source (see *Numbered peering* below).

Owner contract
~~~~~~~~~~~~~~

The topology owner remains responsible for the router, switch, ports, provider
bridge, and the addressing those ports carry. Before stamping
``microovn-bgp-attach=true``, the owner must have configured:

- the router pinned to the local chassis (``options:chassis=<chassis>``),
- an explicit MAC (not ``auto``) on the LRP,
- for numbered peering, the transit IPv4 on the LRP's ``networks`` (see
  *Numbered peering*), and the peer in ``microovn-bgp-peers``.

Everything else is provisioned by MicroOVN itself: ``dynamic-routing=true``
and — when absent — ``dynamic-routing-vrf-id`` on the router, plus
``dynamic-routing-maintain-vrf=true`` and the computed
``dynamic-routing-port-name`` on the LRP. An owner-set
``dynamic-routing-vrf-id`` is respected (so a deployment can pin a
deterministic learn-table); the values MicroOVN writes are journaled and
restored on detach. The owner no longer hand-configures the dynamic-routing
plumbing per node.

Numbered peering
~~~~~~~~~~~~~~~~

To peer *numbered* (IPv4) on an uplink, put the transit IPv4 on the tagged
LRP's ``networks`` — when creating the port use
``ovn-nbctl lrp-add <lr> <lrp> <mac> 10.0.0.1/30`` (or set it afterwards with
``ovn-nbctl set Logical_Router_Port <lrp> networks=10.0.0.1/30``) — and list
the peer inside that network in ``microovn-bgp-peers`` (e.g.
``65002@10.0.0.2`` for a ``10.0.0.1/30`` LRP). There is no dedicated local-IP
key: OVN's ``routing-protocol-redirect`` shares IP and MAC between the LRP and
its redirect port, so the LRP's own address *is* the BGP source. MicroOVN
detects numbered peering from the presence of an IPv4 peer, selects the single
LRP network containing every IPv4 peer as the transit prefix (rejecting peers
outside it or spanning multiple networks), and assigns that network to the
attachment veth.

- A ``/30`` is the recommended point-to-point prefix for peer addresses (the
  network and directed-broadcast addresses of it are rejected, so the two
  usable hosts are the leaf and the LRP). A ``/31`` (RFC 3021) also works, but
  its northd support is new: ``/31`` router ports need central OVN 26.03.3 or
  26.09+ (the fix landed on ``main``/``branch-26.03`` after 26.03.0). On an
  older central (e.g. an archive 26.03.0 snap) a ``/31`` LRP misclassifies the
  peer address as a broadcast and can drop peer-sourced routed traffic, so
  prefer ``/30`` there. A ``/32`` host route is rejected as not a transit
  prefix.
- Numbered uplinks do not stamp the IPv6 RA options; the veth carries a static
  IPv4 instead. RA options stamped by a prior unnumbered attach are simply left
  in place until detach (harmless on a numbered uplink) rather than forcibly
  removed, so an owner's own RA configuration is never disturbed.
- BFD remains passive and, as with unnumbered, applies to every attached peer.
- The peer family is per-uplink: an uplink is either numbered (IPv4 peers)
  or unnumbered (IPv6 link-local peers), never a mix.

The numbered path is covered by an end-to-end integration test
(``tests/bgp_attach.bats``) and by unit tests for the pure helpers.

MicroOVN then attaches BIRD: it creates a veth pair, adds the OVS side to
``br-int``, records the port in ``Open_vSwitch``'s
``dynamic-routing-port-mapping``, adds a redirect LSP on the owner's switch, and
sets ``routing-protocol-redirect`` on the LRP. While attached it sets the LRP's
``routing-protocols`` to ``BGP,BFD`` (mirroring the managed/redirect path) so OVN
redirects both BGP and BFD to the attachment veth; the attach BIRD config carries
a passive BFD listener, so BFD is only used when a peer has it configured and
peers that do not speak BFD are unaffected. BFD is applied to every attached peer
with no per-peer opt-out — this is deliberate: BIRD answers passively, so a peer
without BFD simply never establishes a session, while a BFD-capable peer gets fast
failure detection. The periodic IPv6 RA options
(``send_periodic``, ``address_mode``, ``max_interval``, ``min_interval``) are set
to fixed values for unnumbered BGP and override the owner's. The owner's original
``routing-protocols`` and RA values are recorded and restored on detach. Those
edits are permanent while the attachment is active; the owner should not modify
or delete the redirect LSP. If the owner rewrites ``routing-protocols``
mid-attachment (e.g. drops BFD, or the node upgrades from a pre-BFD MicroOVN), the
reconciler re-stamps it to ``BGP,BFD`` on the next convergence check — otherwise
BFD is never redirected to the veth and fast failure detection silently does not
work for a redundant fabric that expects it. The RA options are likewise kept
mode-aware by convergence: an unnumbered uplink is re-stamped with the RA
options if they drift; a numbered uplink simply does not stamp them (any RA
options stamped by a prior unnumbered attach are left until detach).

If the owner also runs OVN-managed BFD on the same LRP (an NB ``BFD`` row), the
``BGP,BFD`` stamp still coexists: on OVN 26.03+ an owner's BFD traffic is cloned
to the router pipeline rather than stolen, so an owner BFD session is preserved
alongside the redirected one. Below 26.03 the redirect would instead steal an
owner's OVN BFD traffic, but attach never runs against a central older than 26.03
— the reconciler skips it with a warning — so that case is unreachable. This
dual-responder configuration is not tested; prefer letting MicroOVN's redirected
BFD be the only speaker on an attached LRP.

BFD failure detection is sub-second, so live attachments are sensitive to the LRP
``options`` and to datapath interruptions. The attach template leaves BFD at its
defaults (BIRD's own are ``min-rx 10ms`` / ``min-tx 100ms`` / multiplier 5);
the actual
detect time is negotiated from *both* ends' advertised intervals and multiplier —
a typical peer such as FRR defaults to 300ms x 3, i.e. roughly a second. If the
owner rewrites ``routing-protocols``/``routing-protocol-redirect`` mid-attachment,
or the datapath stalls for longer than the detect time (an ``ovn-controller``/OVS
restart, a snap refresh, or any other OVS/``vswitchd`` restart or component
swap), a BFD-capable peer gives up the session and resets BGP. The reconciler
re-stamps on its next tick, so the worst-case recovery is one 30s tick; a peer
that does not run BFD has no session to drop and BGP rides through the gap. BFD
here is passive (BIRD answers a peer that initiates, it never starts one), so a
fully passive peer-and-owner pair never establishes a session — the peer must be
the active BFD speaker.

If a target's addressing is invalid (an IPv4 peer outside the LRP's network, a
``/32`` host route, a mixed-family peer list, or an invalid ASN), the reconciler
skips that target and keeps BIRD on its last good configuration, rather than
tearing a live session down. A single invalid target therefore pauses BIRD
updates for the whole chassis (it is logged every tick until corrected) without
dropping the healthy sessions — fix the marker/peers to resume updates.

The owner must preserve the LRP's own ``microovn-bgp-attach`` marker while attach
is active. A controller that rewrites ``external_ids`` wholesale (as Neutron's
OVN IDL resync does) drops that marker; MicroOVN then sees no desired target and
tears the attachment down, but its ownership journal in the local OVS DB
survives. Neutron/Sunbeam compatibility is not yet validated.

Similarly, while attached the owner must not rewrite the dynamic-routing
plumbing MicroOVN provisions (``dynamic-routing`` / ``dynamic-routing-vrf-id``
on the router, ``dynamic-routing-maintain-vrf`` / ``dynamic-routing-port-name``
on the LRP). ``dynamic-routing``, ``maintain-vrf`` and ``port-name`` are
verified-and-repaired by convergence, so a rewrite of those is re-stamped on the
next tick; a rewritten ``dynamic-routing-vrf-id`` (a change of learn-table
mid-attach) is not converged — the attachment veth stays in the original VRF —
so keep that value stable for the life of the attachment.

Minimum OVN version: attach mode requires OVN 26.03 or newer on the central
(northd) side. The chassis always runs the version shipped by the MicroOVN snap.
The dynamic-routing primitives the attach uses (``routing-protocol-redirect``,
``dynamic-routing-port-name``, ``-maintain-vrf``) arrived in 25.03; the minimum
is pinned at 26.03 because that is where ``options:dynamic-routing-vrf-id`` was
introduced, which attach anchors its VRF-id provisioning on.

.. note::

   ``microovn-bgp-asn`` is used in two distinct places: by ``microovn setup bgp``
   as a dqlite cluster key, and by the reconciler as an ``Open_vSwitch``
   external-id. The two stores are separate. The managed path's enable trigger is
   the ``microovn-bgp-setup`` ``Open_vSwitch`` external-id (a ``bool``-like flag,
   formerly ``microovn-bgp-enabled``); attach mode instead uses the
   per-``Logical_Router_Port`` ``microovn-bgp-attach`` marker (there is no
   chassis-wide enable gate).

Underlay (``enable bird``)
--------------------------

A node may also need to advertise its own underlay addresses (control-plane,
GENEVE, storage loopbacks) to the fabric, independently of managed or attach
mode. ``microovn enable bird`` turns this into a structured mode:

.. code-block:: none

   microovn enable bird \
     --uplink dpu1p1.100 --uplink dpu2p1.100 \
     --config asn=4200000006 --config router_id=0.0.0.6 --config ula=fd2b:5d1a:7c93::/48

Only the uplink interfaces are required; the other scalars are defaulted from the
node name. The node is the *active* speaker: it discovers the ToR's link-local
address from the ToR's Router Advertisements and brings up an unnumbered IPv6
eBGP session per uplink, advertising the node's own loopback ``/128`` routes.
``--stdin`` applies a raw BIRD configuration instead (the former ``setup bird``).

The config is stored at ``$SNAP_COMMON/data/bird/underlay.json``. There is no
``disable bird`` in the current release: to remove the underlay, delete that file
and stop/restart ``microovn.bird``. Raw ``setup bird``/``--stdin`` requires an
OVS/OVN-capable node (it refuses while attach or an underlay is active).

Disable BGP integration
-----------------------

To disable BGP integration, simply run:

.. code-block:: none

   microovn disable bgp

This will remove all VRF tables, virtual interfaces, OVS bridges, Logical
Switches and Logical Routers that were created when the managed BGP integration
was enabled.

MicroOVN will also backup and reset BIRD startup configuration. The current
configuration file will be backed up in the same directory under name
``bird.conf_<unix_timestamp>`` and then replaced with the default
BIRD configuration.

.. LINKS
.. _VRF: https://docs.kernel.org/networking/vrf.html
.. _RFC 6996: https://datatracker.ietf.org/doc/html/rfc6996
