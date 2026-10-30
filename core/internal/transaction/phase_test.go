package transaction

import "testing"

func TestPhaseTransitions(t *testing.T) {
	if err := ValidateTransition(PhasePrepared, PhaseComposeWrite); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTransition(PhaseVerified, PhaseCommitted); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTransition(PhaseCommitted, PhaseApplying); err == nil {
		t.Fatal("committed must be terminal")
	}
	if err := ValidateTransition(PhaseRollbackPending, PhaseNeedsRepair); err != nil {
		t.Fatal(err)
	}
}
