package core

import (
	"fmt"
	"sort"
	"sync"
)

// BIP9-style version-bit signaling.
//
// upgrade.go activates a rule at a height an operator configures: a flag day.
// That works, and it puts the whole burden of coordination outside the protocol —
// every node must be told the same number by hand, and nothing checks that the
// hashpower actually producing blocks is running code that understands the new
// rule. Activate too early and the miners fork; activate too late and the change
// waits on the slowest operator to edit a config file.
//
// Version bits move that coordination on-chain. A deployment claims one bit of
// the block header's Version field. Miners that are ready set the bit; when a
// window of blocks contains enough of them, the rule LOCKS IN, and it becomes
// ACTIVE one whole window later — so every node learns the activation height
// from the chain itself, with a window of warning, and the threshold is evidence
// that the hashpower which will mine under the new rule can already produce it.
//
// How this joins the existing mechanism: a locked-in deployment has a known
// activation HEIGHT, which is exactly what upgrade.go already consumes. So the
// chain evaluates its deployments whenever the tip moves and installs that height
// via SetUpgradeHeight — every validation rule keeps asking IsUpgradeActive and
// needs no notion of signaling at all. A reorg that unwinds a lock-in withdraws
// the height again, so the rule follows the chain rather than the order in which
// blocks happened to arrive.
//
// A name is either height-scheduled by an operator or bit-deployed, never both:
// RegisterDeployment takes ownership of the name.

// Version top bits. BIP9 reserves the high three bits as 001 to mark a version
// as carrying signaling, which leaves the remaining 29 as assignable bits and
// keeps a plain version number from being read as a vote.
const (
	VersionTopMask uint32 = 0xE000_0000
	VersionTopBits uint32 = 0x2000_0000
	// MaxDeploymentBit is the highest assignable bit (0..28).
	MaxDeploymentBit uint8 = 28
)

// DeploymentState is where a deployment stands at some height.
type DeploymentState string

const (
	// DeploymentDefined means signaling has not begun (below Start).
	DeploymentDefined DeploymentState = "defined"
	// DeploymentStarted means miners may signal and windows are being counted.
	DeploymentStarted DeploymentState = "started"
	// DeploymentLockedIn means a window met the threshold; activation is scheduled
	// one window later and can no longer be withdrawn by miners changing their vote.
	DeploymentLockedIn DeploymentState = "locked_in"
	// DeploymentActive means the rule is in force.
	DeploymentActive DeploymentState = "active"
	// DeploymentFailed means the timeout passed without a window meeting the
	// threshold. It is terminal: the deployment must be re-proposed with new
	// heights rather than lingering as a bit miners might set years later.
	DeploymentFailed DeploymentState = "failed"
)

// Deployment is one proposed rule change and the terms of its vote.
type Deployment struct {
	// Name is the upgrade this deployment activates (see upgrade.go).
	Name string `json:"name"`
	// Bit is the header Version bit miners set to signal readiness (0..28).
	Bit uint8 `json:"bit"`
	// Start is the first height of the first counting window.
	Start uint64 `json:"start"`
	// Timeout is the height past which a deployment that has not locked in fails.
	Timeout uint64 `json:"timeout"`
	// Window is how many blocks one counting window spans.
	Window uint64 `json:"window"`
	// Threshold is how many blocks in a window must signal for it to lock in.
	Threshold uint64 `json:"threshold"`
}

// Validate reports whether a deployment's terms are self-consistent. A
// deployment that can never lock in (threshold above the window) or never even
// start (timeout before the first window closes) is a configuration mistake that
// would otherwise look exactly like a change nobody voted for.
func (d Deployment) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("deployment has no name")
	}
	if !KnownUpgrade(d.Name) {
		return fmt.Errorf("unknown upgrade %q (known: %v)", d.Name, Upgrades())
	}
	if d.Bit > MaxDeploymentBit {
		return fmt.Errorf("deployment %s: bit %d out of range (0..%d)", d.Name, d.Bit, MaxDeploymentBit)
	}
	if d.Window == 0 {
		return fmt.Errorf("deployment %s: window must be non-zero", d.Name)
	}
	if d.Threshold == 0 || d.Threshold > d.Window {
		return fmt.Errorf("deployment %s: threshold %d must be in 1..%d (the window)", d.Name, d.Threshold, d.Window)
	}
	if d.Timeout < d.Start+d.Window {
		return fmt.Errorf("deployment %s: timeout %d is before the first window closes (%d)", d.Name, d.Timeout, d.Start+d.Window)
	}
	return nil
}

// Signals reports whether this header votes for the given bit. A version whose
// top bits are not 001 carries no vote at all, so an old miner emitting version 0
// — or a future scheme using the high bits for something else — is never counted
// as supporting a change it has never heard of.
func (h Header) Signals(bit uint8) bool {
	if bit > MaxDeploymentBit {
		return false
	}
	return h.Version&VersionTopMask == VersionTopBits && h.Version&(1<<bit) != 0
}

// SignalVersion is the header version a miner should mine with to signal the
// given bits (none of them: a plain signaling version that votes for nothing).
func SignalVersion(bits ...uint8) uint32 {
	v := VersionTopBits
	for _, b := range bits {
		if b <= MaxDeploymentBit {
			v |= 1 << b
		}
	}
	return v
}

var (
	deploymentsMu sync.RWMutex
	deployments   = map[string]Deployment{}
)

// RegisterDeployment puts a rule change to a miner vote. Call it at startup with
// identical terms on every node, before syncing; the terms are consensus, exactly
// as an activation height is.
func RegisterDeployment(d Deployment) error {
	if err := d.Validate(); err != nil {
		return err
	}
	deploymentsMu.Lock()
	defer deploymentsMu.Unlock()
	for _, other := range deployments {
		if other.Name != d.Name && other.Bit == d.Bit {
			return fmt.Errorf("bit %d is already claimed by deployment %q", d.Bit, other.Name)
		}
	}
	deployments[d.Name] = d
	return nil
}

// Deployments lists the registered deployments, ordered by name.
func Deployments() []Deployment {
	deploymentsMu.RLock()
	defer deploymentsMu.RUnlock()
	out := make([]Deployment, 0, len(deployments))
	for _, d := range deployments {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ClearDeployments removes every registered deployment (used by tests).
func ClearDeployments() {
	deploymentsMu.Lock()
	defer deploymentsMu.Unlock()
	deployments = map[string]Deployment{}
}

// DeploymentStatus is a deployment plus where the chain has taken it.
type DeploymentStatus struct {
	Deployment
	State DeploymentState `json:"state"`
	// Since is the height at which the current state began.
	Since uint64 `json:"since"`
	// Activation is the height the rule takes effect, known once the deployment
	// locks in and 0 before that.
	Activation uint64 `json:"activation"`
	// WindowStart, Signals and Elapsed describe the window in progress at the tip,
	// so an operator can see a vote failing before the window closes rather than
	// after.
	WindowStart uint64 `json:"window_start"`
	Signals     uint64 `json:"signals"`
	Elapsed     uint64 `json:"elapsed"`
}

// evaluateDeployment walks the counting windows and returns where a deployment
// stands at the tip of `headers` (indexed by height).
//
// The state is constant across a whole window and each window's state is decided
// by the one before it, which is what makes the result identical on every node:
// it depends only on blocks that are already mined, never on how far into the
// current window the tip happens to be.
func evaluateDeployment(headers []Header, d Deployment) DeploymentStatus {
	st := DeploymentStatus{Deployment: d, State: DeploymentDefined, Since: 0}
	if len(headers) == 0 {
		return st
	}
	tip := headers[len(headers)-1].Index

	signalsIn := func(from uint64) uint64 {
		var n uint64
		for h := from; h < from+d.Window; h++ {
			if int(h) < len(headers) && headers[h].Signals(d.Bit) {
				n++
			}
		}
		return n
	}

	for k := uint64(0); ; k++ {
		winStart := d.Start + k*d.Window
		if winStart > tip {
			break
		}
		st.WindowStart = winStart
		if k == 0 {
			st.State, st.Since = DeploymentStarted, winStart
			continue
		}
		prevStart := winStart - d.Window
		switch st.State {
		case DeploymentStarted:
			if signalsIn(prevStart) >= d.Threshold {
				st.State, st.Since = DeploymentLockedIn, winStart
				st.Activation = winStart + d.Window
			} else if winStart > d.Timeout {
				st.State, st.Since = DeploymentFailed, winStart
			}
		case DeploymentLockedIn:
			st.State, st.Since = DeploymentActive, winStart
			st.Activation = winStart
		}
	}
	if st.State != DeploymentDefined {
		st.Elapsed = tip - st.WindowStart + 1
		st.Signals = signalsIn(st.WindowStart)
	}
	return st
}

// DeploymentStatuses reports where every registered deployment stands on this
// chain. It is what /deployments serves and what `dnas node deployments` prints.
func (bc *Blockchain) DeploymentStatuses() []DeploymentStatus {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.deploymentStatusesLocked()
}

func (bc *Blockchain) deploymentStatusesLocked() []DeploymentStatus {
	regd := Deployments()
	if len(regd) == 0 {
		return nil
	}
	headers := make([]Header, len(bc.blocks))
	for i, b := range bc.blocks {
		headers[i] = b.Header()
	}
	out := make([]DeploymentStatus, 0, len(regd))
	for _, d := range regd {
		out = append(out, evaluateDeployment(headers, d))
	}
	return out
}

// refreshDeploymentsLocked re-derives every deployment's activation height from
// the chain and installs it (or withdraws it) in the upgrade table.
//
// It runs after any change to the block list — a connect, a reorg, or opening a
// store — because a reorg can undo a lock-in: the winning branch may simply not
// contain the window that met the threshold, and a rule that stayed active on the
// strength of a discarded branch would have this node validating against a chain
// nobody else is on.
func (bc *Blockchain) refreshDeploymentsLocked() {
	for _, st := range bc.deploymentStatusesLocked() {
		switch st.State {
		case DeploymentLockedIn, DeploymentActive:
			SetUpgradeHeight(st.Name, st.Activation)
		default:
			ClearUpgradeHeight(st.Name)
		}
	}
}
