package transaction

import "fmt"

// Phase is the durable phase of a cross-resource operation.
type Phase string

const (
	PhasePlanned            Phase = "planned"
	PhasePrepared           Phase = "prepared"
	PhaseApplying           Phase = "applying"
	PhaseProxyUpdated       Phase = "proxy_updated"
	PhaseComposeWrite       Phase = "compose_write_pending"
	PhaseComposeWritten     Phase = "compose_written"
	PhaseComposeUpdated     Phase = "compose_updated"
	PhaseContainerReady     Phase = "container_ready"
	PhaseContainerRecreated Phase = "container_recreated"
	PhaseGatewayReady       Phase = "gateway_ready"
	PhaseVerifying          Phase = "verifying"
	PhaseVerified           Phase = "verified"
	PhaseCommittedItem      Phase = "committed_item"
	PhaseCommitted          Phase = "committed"
	PhaseRollbackPending    Phase = "rollback_pending"
	PhaseRolledBack         Phase = "rolled_back"
	PhaseRollbackFailed     Phase = "rollback_failed"
	PhaseNeedsRepair        Phase = "needs_repair"
)

// CanTransition reports whether phase b is a legal successor of a. Recovery
// may jump from any non-terminal phase to a recovery branch.
func CanTransition(a, b Phase) bool {
	if a == "" {
		return b == PhasePlanned || b == PhasePrepared
	}
	if terminal(a) {
		return false
	}
	if b == PhaseRollbackPending || b == PhaseRolledBack || b == PhaseRollbackFailed || b == PhaseNeedsRepair {
		return true
	}
	switch a {
	case PhasePlanned:
		return b == PhasePrepared || b == PhaseApplying
	case PhasePrepared:
		return b == PhaseApplying || b == PhaseComposeWrite
	case PhaseApplying:
		return b == PhaseProxyUpdated || b == PhaseComposeWrite || b == PhaseVerifying
	case PhaseProxyUpdated:
		return b == PhaseComposeWrite || b == PhaseVerifying || b == PhaseComposeUpdated
	case PhaseComposeWrite:
		return b == PhaseComposeWritten || b == PhaseComposeUpdated || b == PhaseVerifying || b == PhaseRollbackPending
	case PhaseComposeWritten:
		return b == PhaseContainerReady || b == PhaseContainerRecreated || b == PhaseGatewayReady || b == PhaseVerifying || b == PhaseRollbackPending
	case PhaseComposeUpdated:
		return b == PhaseContainerReady || b == PhaseContainerRecreated || b == PhaseGatewayReady || b == PhaseRollbackPending
	case PhaseContainerReady:
		return b == PhaseGatewayReady || b == PhaseVerified || b == PhaseRollbackPending
	case PhaseContainerRecreated:
		return b == PhaseGatewayReady || b == PhaseVerified || b == PhaseRollbackPending
	case PhaseGatewayReady:
		return b == PhaseVerifying || b == PhaseVerified || b == PhaseRollbackPending
	case PhaseVerifying:
		return b == PhaseVerified || b == PhaseRollbackPending
	case PhaseVerified:
		return b == PhaseCommittedItem || b == PhaseCommitted || b == PhaseRollbackPending
	case PhaseCommittedItem:
		return false
	case PhaseRollbackPending:
		return b == PhaseRolledBack || b == PhaseRollbackFailed || b == PhaseNeedsRepair
	default:
		return false
	}
}

func terminal(p Phase) bool {
	switch p {
	case PhaseCommitted, PhaseRolledBack, PhaseRollbackFailed:
		return true
	}
	return false
}

// ValidateTransition returns a descriptive error for an illegal transition.
func ValidateTransition(from, to Phase) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("非法事务阶段转换: %s → %s", from, to)
	}
	return nil
}
