package agent

import "testing"

func TestComputerUseAlternateControlRecoveryRequiresNoCommittedEffect(
	t *testing.T,
) {
	valid := ComputerUseTaskOutcome{
		Status:   ComputerUseTaskNotCompleted,
		Effect:   ComputerUseCommitNone,
		Recovery: ComputerUseRecoveryAlternateControl,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid alternate-control recovery: %v", err)
	}
	for _, invalid := range []ComputerUseTaskOutcome{
		{
			Status:   ComputerUseTaskCompleted,
			Effect:   ComputerUseCommitNone,
			Recovery: ComputerUseRecoveryAlternateControl,
		},
		{
			Status:   ComputerUseTaskNotCompleted,
			Effect:   ComputerUseCommitKnown,
			Recovery: ComputerUseRecoveryAlternateControl,
		},
		{
			Status:   ComputerUseTaskUnverified,
			Effect:   ComputerUseCommitUnknown,
			Recovery: ComputerUseRecoveryAlternateControl,
		},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid alternate-control recovery accepted: %+v", invalid)
		}
	}
}
