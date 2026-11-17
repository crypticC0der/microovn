================================================
Select the Open vSwitch engine (DOCA acceleration)
================================================

MicroOVN ships two builds of Open vSwitch as snap `components`_, and an
inference layer picks the one most suitable for the host:

- ``ovs`` — the standard kernel-datapath build. Selected everywhere by
  default.
- ``ovs-doca`` — the NVIDIA DOCA-accelerated build. Selected on hosts with a
  compatible Mellanox ConnectX or BlueField device.

The best engine is chosen automatically at install and refresh time, so most
deployments require no manual action.

.. _components: https://snapcraft.io/docs/components

.. note::

   This feature is experimental and the selection mechanics are likely to
   change before it is finalised.

Building the snap
-----------------

This section is only relevant when building from source. A published snap
already carries both components.

Open vSwitch engines are shipped as snap components alongside the main snap.
Building the snap produces all three artifacts:

.. code-block:: none

   snapcraft pack --use-lxd

The output includes ``microovn_<version>_amd64.snap``, ``microovn+ovs.comp``
and ``microovn+ovs-doca.comp``. Publish or install all three together; a
component on its own does not provide a working MicroOVN.

.. note::

   Build from a real ``git clone`` rather than a worktree: ``snapcraft``
   derives the snap version from ``git describe``, which fails in a worktree.

.. note::

   DOCA is only available on ``amd64``. On other architectures the
   ``ovs-doca`` component and its package repository are skipped and only the
   ``ovs`` engine is produced.

Installing
----------

Install the snap and both components together:

.. code-block:: none

   snap install --dangerous ./microovn_*.snap \
       ./microovn+ovs.comp ./microovn+ovs-doca.comp

For a store-published snap a plain ``snap install microovn`` installs the
components automatically; the explicit ``.comp`` arguments above are only
needed for locally built (``--dangerous``) snaps.

The interactive ``microovn inference`` command reads host hardware to rank
the engines. ``hardware-observe`` is a manual-connect interface, so grant it
before using the command interactively:

.. code-block:: none

   snap connect microovn:hardware-observe

The install and post-refresh hooks connect ``hardware-observe`` for their own
use and do not need this step.

Engine selection
----------------

Selection happens automatically: both the install and post-refresh hooks run
``microovn inference auto --fallback ovs``. The fallback is used when host
hardware information is unavailable, or when no engine matches.

To inspect the engines and how they score on this host:

.. code-block:: none

   microovn inference list

This prints each engine's name, whether it is compatible, its score and
whether it is currently active.

To select an engine manually:

.. code-block:: none

   microovn inference auto --fallback ovs
   microovn inference status

``microovn inference auto`` activates the highest-scoring compatible engine,
records the choice and installs the component the engine requires. The
``switch`` service reads the recorded engine and runs that component's
``ovs-vswitchd`` with its bundled libraries.

Enabling the DOCA-accelerated datapath
--------------------------------------

Selecting the ``ovs-doca`` engine makes MicroOVN run NVIDIA's build of Open
vSwitch, but the accelerated ``doca`` datapath is not provided by the snap.
It requires the NVIDIA DOCA software stack on the host itself:

- NVIDIA DOCA-OFED kernel drivers for the device.
- ``doca-init`` completing the accelerated datapath setup.
- 1 GiB huge pages reserved for the datapath.
- IOMMU enabled.

The host must be rebooted after the drivers are installed for the modules to
load. Until those prerequisites are in place, the ``doca`` datapath type is
not available and Open vSwitch continues on the kernel or netdev datapath
even though the DOCA build is running.

The ``ovs-doca`` engine is only considered on ``amd64`` hosts with a
compatible device (PCI vendor ``0x15b3``). On every other host ``ovs`` is
selected.