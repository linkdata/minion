package action

// criticPolicy replays worker-reported verdicts without treating token accounting as approval.
type criticPolicy struct {
	job                    Job
	verificationApproved   bool
	verificationNoBlockers bool
	noBlockerRounds        int
}

func (p *criticPolicy) observe(call CallUsage) (advanceToFull bool, stopReason string) {
	if call.Role == roleMain {
		p.verificationApproved = call.Round == 1
		p.verificationNoBlockers = call.Round == 1
		return false, ""
	}
	if call.Role != roleCritic {
		return false, ""
	}
	if !call.ReviewComplete {
		p.noBlockerRounds = 0
		return false, ""
	}
	if call.ReviewPhase == reviewVerification {
		p.verificationApproved = call.Approved
		p.verificationNoBlockers = call.NoBlockers
		if !call.NoBlockers {
			p.noBlockerRounds = 0
		}
		return call.NoBlockers || call.Round == p.job.rounds(), ""
	}
	if call.ReviewPhase != reviewFull {
		return false, ""
	}
	if !call.NoBlockers {
		p.noBlockerRounds = 0
	} else {
		p.noBlockerRounds++
		if call.Approved && p.verificationApproved && p.verificationNoBlockers {
			return false, "approved"
		}
		if p.verificationNoBlockers && p.job.CriticNonblockingRounds > 0 && call.Round >= p.job.CriticNonblockingRounds &&
			(p.job.CriticConsecutiveNoBlockerRounds == 0 || p.noBlockerRounds >= p.job.CriticConsecutiveNoBlockerRounds) {
			return false, "nonblocking"
		}
	}
	if call.Round == p.job.rounds() {
		return false, "round_cap"
	}
	return false, ""
}
