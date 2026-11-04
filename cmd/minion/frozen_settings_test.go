package main

import (
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
)

var frozenTestCodex = config.RoleModelConfig{Model: "main-1", Effort: "high"}

func frozenFailureForTest(accepted state.AcceptedAction) state.JobFailure {
	return state.JobFailure{
		Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
		MainProvider: accepted.MainProvider, CriticProvider: accepted.CriticProvider,
		MainSettings: accepted.MainSettings, CriticSettings: accepted.CriticSettings,
	}
}
