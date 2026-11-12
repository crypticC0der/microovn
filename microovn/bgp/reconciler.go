package bgp

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/canonical/lxd/shared/logger"
	microTypes "github.com/canonical/microcluster/v3/microcluster/types"

	"github.com/canonical/microovn/microovn/api/types"
	"github.com/canonical/microovn/microovn/database"
	ovnCmd "github.com/canonical/microovn/microovn/ovn/cmd"
	"github.com/canonical/microovn/microovn/snap"
)

// attachReconcileInterval is how often the attach reconciler re-discovers the
// desired state from the external-id markers and converges to it.
const attachReconcileInterval = 30 * time.Second

var reconcilerOnce sync.Once

// reconcileMu serialises attach passes. The background ticker and
// `microovn setup bgp-attach` both drive ReconcileAttach/DisableAttach, and two
// concurrent passes could race on creating or removing the same veth and LSP.
var reconcileMu sync.Mutex

// StartReconciler starts the attach reconciler goroutine once per daemon
// process. It is called from several lifecycle hooks (OnStart and after
// bootstrap/join), guarded by a sync.Once so at most one goroutine runs. The
// goroutine lives for the daemon's lifetime; it uses its own context that still
// carries a logger, because the microcluster database layer fetches one from the
// context and errors with "Logger does not exist on context" otherwise.
func StartReconciler(s microTypes.State) {
	reconcilerOnce.Do(func() {
		logger.Info("Starting BGP attach reconciler")
		go func() {
			ctx := microTypes.ContextWithLogger(context.Background())
			ticker := time.NewTicker(attachReconcileInterval)
			defer ticker.Stop()
			for {
				if err := ReconcileAttach(ctx, s); err != nil {
					logger.Warnf("BGP attach reconciliation failed: %v", err)
				}
				<-ticker.C
			}
		}()
	})
}

// ReconcileAttach performs one converge pass: it reads the attach markers and
// knobs from OVS/OVN and reconciles the running state to match (attach new,
// detach removed, re-apply changed, render BIRD). It is safe to call repeatedly.
func ReconcileAttach(ctx context.Context, s microTypes.State) error {
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	return reconcileAttach(ctx, s)
}

func reconcileAttach(ctx context.Context, s microTypes.State) error {
	// Backstop for the PostRemove hook: drop the rack labels of members that
	// left the cluster. It only reads when nobody has left.
	if err := PruneDepartedMembers(ctx, s); err != nil {
		logger.Warnf("BGP attach: failed to drop departed members' rack labels: %v", err)
	}

	// Attach only makes sense on a node running the OVS switch. Skip the whole
	// pass cheaply otherwise, so central-only nodes don't block on the (30s)
	// switch DB wait every tick and log reconciler failures for a feature they
	// haven't enabled.
	if !switchServiceActive(ctx, s) {
		return nil
	}

	// Turn a stored `setup bgp-attach` intent into this chassis's markers (or
	// undo them once the intent is gone). A failure here is logged, not fatal:
	// hand-stamped markers keep working through the pass below either way.
	if err := reconcileAttachIntent(ctx, s); err != nil {
		logger.Warnf("BGP attach intent: %v", err)
	}

	gate, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", BgpEnableGate)
	if err != nil {
		return fmt.Errorf("failed to read %s gate: %w", BgpEnableGate, err)
	}

	if gate != "enabled" {
		return disableAttach(ctx, s)
	}

	// Mutual exclusion: managed (own-topology) BGP and attach must not both
	// drive a node. If we can't confirm the managed path is absent (an NB blip),
	// skip this tick conservatively rather than risk both paths driving BIRD.
	managed, err := HasManagedBgp(ctx, s)
	if err != nil {
		logger.Warnf("Failed to check whether managed BGP is active, skipping attach: %v", err)
		return nil
	}
	if managed {
		return nil
	}

	targets, tagged, foreign, err := discoverAttachTargets(ctx, s)
	if err != nil {
		return err
	}
	taggedSet := map[string]bool{}
	for _, l := range tagged {
		taggedSet[l] = true
	}
	foreignSet := map[string]bool{}
	for _, l := range foreign {
		foreignSet[l] = true
	}

	owned, err := ownedAttachPorts(ctx, s)
	if err != nil {
		return err
	}

	// Detach only owned ports that are affirmatively not-ours: the marker was
	// removed (not in "tagged") or the router moved to another chassis (in
	// "foreign"). A port still tagged but unresolved this pass (a transient NB
	// read error) is kept, so a transient failure never tears down a live BGP
	// session.
	detachedAny := false
	for _, routerPort := range owned {
		if !taggedSet[routerPort] || foreignSet[routerPort] {
			if detachErr := detachPort(ctx, s, routerPort); detachErr != nil {
				logger.Warnf("Failed to detach %s: %v", routerPort, detachErr)
			} else {
				detachedAny = true
			}
		}
	}

	if len(targets) == 0 {
		// No resolved local targets this pass. Stop BIRD only when every tagged
		// port is affirmatively foreign (or there are none) AND we either tore
		// something down this pass or BIRD still runs an attach-generated
		// config (e.g. a previous teardown failed). A tagged port that failed
		// to resolve transiently stays in "tagged" but not "foreign", so BIRD
		// survives an NB/leader-election blip. The idle gate-on/marker-free
		// state stays quiet (no snapd churn, no `setup bird` clobber).
		noLocal := len(tagged) == len(foreign)
		if noLocal && (detachedAny || birdConfigIsAttach()) {
			return stopAndResetBird(ctx)
		}
		return nil
	}

	// Attach requires OVN >= 26.03 on the central side; skip (never hard-fail)
	// if we can't prove that.
	supports, northdVersion, err := centralSupportsAttach(ctx, s)
	if err != nil {
		logger.Warnf("Unable to determine central OVN version, skipping attach: %v", err)
		return nil
	}
	if !supports {
		logger.Warnf("Central OVN is older than 26.03; skipping attach reconciliation")
		return nil
	}
	if !supportsSlash31LRP(northdVersion) {
		// A /31 LRP is unsafe on this central (peer address treated as the
		// broadcast). Warn so an operator using /31 knows to switch to /30; the
		// session still forms, so this is advisory.
		for _, t := range targets {
			if t.LocalIP.IsValid() && t.LocalIP.Bits() == 31 {
				logger.Warnf("router port %s uses a /31 transit prefix, which needs central OVN >= 26.03.3 (central is %s); prefer a /30", t.RouterPort, northdVersion)
			}
		}
	}

	routerID, asn, advertise, imp, err := readAttachKnobs(ctx, s)
	if err != nil {
		return err
	}

	// advertise-table is required in attach mode and is NOT autogenerated: it
	// is the table the topology owner / BGP producer populates with /32s. An
	// auto-picked table nobody writes to would silently advertise nothing.
	if advertise == 0 {
		return fmt.Errorf("attach mode requires a populated %s external-id", attachAdvertiseKey)
	}

	// v1 collapses to a single VRF per chassis, so learn the table id from the
	// first target's router (all targets share it) and derive defaults for the
	// remaining knobs.
	learn, err := resolveLearnTable(ctx, s, targets[0].Router)
	if err != nil {
		return err
	}
	if advertise == learn {
		return fmt.Errorf("advertise-table %d must differ from the VRF learn-table %d", advertise, learn)
	}

	if routerID == "" {
		routerID = generateBGPRouterID(s.Name())
	}
	if asn == 0 {
		asnStr, err := generateAsnFromClusterMemberID(ctx, s, [2]uint64{4210000000, 4294967294})
		if err != nil {
			return err
		}
		n, err := strconv.ParseUint(asnStr, 10, 32)
		if err != nil {
			return fmt.Errorf("failed to parse auto-selected ASN %q: %w", asnStr, err)
		}
		asn = uint32(n)
	}

	cfg := attachConfig{
		RouterID:       routerID,
		LocalASN:       asn,
		AdvertiseTable: advertise,
		LearnTable:     learn,
		ImportPolicy:   imp,
		Targets:        targets,
	}

	// Attach or re-attach each desired target. Preflight runs only for targets we
	// don't yet own; every target (owned or not) is then passed through the
	// idempotent attachUplink, which verifies-and-repairs so a partially failed
	// attach from an earlier tick re-converges. Errors are isolated per target:
	// one bad target must not abort the healthy ones or their BIRD render.
	ownedSet := map[string]bool{}
	for _, routerPort := range owned {
		ownedSet[routerPort] = true
	}
	var converged []attachTarget
	for _, t := range targets {
		// Addressing validation runs for every target, owned or not: preflight
		// only gates new targets, so an owner editing the LRP networks/peers on
		// a live attachment must still be caught (mode/address flips, family
		// changes). A target that fails validation is skipped this tick, which
		// keeps BIRD on its last good config (the tagged loop below sees the
		// redirect is still ours and skips the render) — a single owner typo
		// must not tear down the other healthy uplinks' sessions.
		if err := validateAttachAddressing(t.Peers, t.LocalIP, cfg.LocalASN); err != nil {
			logger.Warnf("attach addressing invalid for %s: %v", t.RouterPort, err)
			continue
		}
		if !ownedSet[t.RouterPort] {
			if alreadyAttached(ctx, s, t) {
				// A previous attach whose local journal was lost: re-journal and
				// let attachUplink repair rather than failing preflight.
				if err := addOwnedPort(ctx, s, t.RouterPort); err != nil {
					logger.Warnf("failed to re-journal %s: %v", t.RouterPort, err)
					continue
				}
				ownedSet[t.RouterPort] = true
			} else if err := preflightAttach(ctx, s, cfg, t); err != nil {
				logger.Warnf("attach preflight failed for %s: %v", t.RouterPort, err)
				continue
			}
		}
		if err := attachUplink(ctx, s, cfg, t); err != nil {
			logger.Warnf("failed to attach %s: %v", t.RouterPort, err)
			continue
		}
		converged = append(converged, t)
	}

	// Render BIRD only if no previously-attached target failed transiently. A
	// transient failure must not drop a previously-attached peer: if any
	// still-tagged port whose redirect points at our LSP failed to attach this
	// pass, keep the last-rendered config. New or partially-attached ports (no
	// redirect yet) are simply skipped and must not block the render; peers for
	// affirmatively-removed ports are dropped in the detach pass above.
	convergedSet := make(map[string]bool, len(converged))
	for _, t := range converged {
		convergedSet[t.RouterPort] = true
	}
	for _, l := range tagged {
		if foreignSet[l] || convergedSet[l] {
			continue
		}
		if ours, err := attachRedirectIsOurs(ctx, s, l); err != nil || ours {
			return nil
		}
	}

	reg := cfg
	reg.Targets = converged
	return writeBirdConfig(ctx, s, reg)
}

// DisableAttach tears down all attachments owned by this chassis and stops
// BIRD. It is the full-teardown path when the enable gate is removed.
func DisableAttach(ctx context.Context, s microTypes.State) error {
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	return disableAttach(ctx, s)
}

func disableAttach(ctx context.Context, s microTypes.State) error {
	// ownedAttachPorts reads the local OVS journal, so it is cheap and NB-free;
	// the idle path here does not touch NB at all.
	owned, err := ownedAttachPorts(ctx, s)
	if err != nil {
		logger.Warnf("Failed to list owned BGP attachments: %v", err)
		return err
	}

	if len(owned) == 0 {
		// Nothing for this chassis to tear down, but a previously-failed stop may
		// still have BIRD running on an attach-generated config; retry the
		// teardown then. Otherwise leave BIRD alone so the steady state doesn't
		// churn the snap service every reconcile.
		if birdConfigIsAttach() {
			return stopAndResetBird(ctx)
		}
		return nil
	}

	var allErr error
	for _, routerPort := range owned {
		if detachErr := detachPort(ctx, s, routerPort); detachErr != nil {
			logger.Warnf("Failed to detach %s: %v", routerPort, detachErr)
			if allErr == nil {
				allErr = detachErr
			}
		}
	}

	if err := stopAndResetBird(ctx); err != nil {
		if allErr == nil {
			allErr = err
		}
	}

	return allErr
}

// stopAndResetBird stops BIRD and resets its config so a stopped daemon isn't
// left with a config that references removed veths. The config is only reset
// after a successful stop: if the stop fails, the attach-generated config (and
// its marker) stays in place so the next tick can retry the teardown via
// birdConfigIsAttach rather than silently leaving BIRD running on a default
// config.
func stopAndResetBird(ctx context.Context) error {
	if err := snap.Stop(ctx, BirdService, true); err != nil {
		logger.Warnf("Failed to stop BIRD: %v", err)
		return err
	}
	return resetBirdConfig(ctx)
}

// switchServiceActive reports whether the local node has the OVS switch service
// enabled (i.e. it is a chassis, not a central-only node).
func switchServiceActive(ctx context.Context, s microTypes.State) bool {
	name := s.Name()
	active := false
	err := s.Database().Transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		services, err := database.GetServices(ctx, tx, database.ServiceFilter{Member: &name})
		if err != nil {
			return err
		}
		for _, svc := range services {
			if svc.Service == types.SrvSwitch {
				active = true
			}
		}
		return nil
	})
	return err == nil && active
}

// HasAttach reports whether attach mode is enabled on this node: a bgp-attach
// intent is stored, or the chassis gate is on and at least one attach marker
// exists.
func HasAttach(ctx context.Context, s microTypes.State) (bool, error) {
	// A stored `setup bgp-attach` intent counts as attach being on, even before
	// this node has derived its markers from it.
	if intent, err := HasAttachIntent(ctx, s); err != nil {
		return false, err
	} else if intent {
		return true, nil
	}

	gate, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", BgpEnableGate)
	if err != nil {
		return false, err
	}
	if gate != "enabled" {
		return false, nil
	}
	targets, _, _, err := discoverAttachTargets(ctx, s)
	if err != nil {
		return false, err
	}
	return len(targets) > 0, nil
}

// HasManagedBgp reports whether the managed (own-topology) BGP path is active,
// detected by the presence of a microovn-created router for this chassis.
func HasManagedBgp(ctx context.Context, s microTypes.State) (bool, error) {
	out, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=name",
		"find", "Logical_Router", fmt.Sprintf("external_ids:%s=%s", BgpManagedTag, getBgpManagedTagValue(s)))
	if err != nil {
		return false, err
	}
	names := parseOvnFind(out)
	for _, name := range names {
		if strings.HasPrefix(name, "lr-"+s.Name()+"-microovn") {
			return true, nil
		}
	}
	return false, nil
}
