package action

// criticPolicy replays verdict evidence without treating token-accounting coverage as approval.
type criticPolicy struct {
	job                  Job
	verificationApproved bool
	noBlockerRounds      int
}

func (p *criticPolicy) observe(call CallUsage) (advanceToFull bool, stopReason string) {
	if call.Role == roleMain {
		p.verificationApproved = call.Round == 1
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
		if !call.NoBlockers {
			p.noBlockerRounds = 0
		}
		return call.NoBlockers, ""
	}
	if call.ReviewPhase != reviewFull {
		return false, ""
	}
	if !call.NoBlockers {
		p.noBlockerRounds = 0
		return false, ""
	}
	p.noBlockerRounds++
	if call.Approved && p.verificationApproved {
		return false, "approved"
	}
	if p.job.CriticNonblockingRounds > 0 && call.Round >= p.job.CriticNonblockingRounds &&
		(p.job.CriticConsecutiveNoBlockerRounds == 0 || p.noBlockerRounds >= p.job.CriticConsecutiveNoBlockerRounds) {
		return false, "nonblocking"
	}
	return false, ""
}
