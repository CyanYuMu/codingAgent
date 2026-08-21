package permission

import "testing"

func TestResolve(t *testing.T) {
	cases := []struct {
		tier Tier
		mode Mode
		want Decision
	}{
		{TierRead, ModeYolo, DecisionAllow},
		{TierExec, ModeYolo, DecisionAllow},
		{TierRead, ModeWrite, DecisionAllow},
		{TierWrite, ModeWrite, DecisionAllow},
		{TierExec, ModeWrite, DecisionPrompt},
		{TierRead, ModeAlwaysAsk, DecisionAllow},
		{TierWrite, ModeAlwaysAsk, DecisionPrompt},
		{TierExec, ModeAlwaysAsk, DecisionPrompt},
	}
	for _, c := range cases {
		if got := Resolve(c.tier, c.mode); got != c.want {
			t.Errorf("Resolve(%v,%v) = %v, want %v", c.tier, c.mode, got, c.want)
		}
	}
}
