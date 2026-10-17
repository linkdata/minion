package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCriticNonblockingRoundsDefaultsAndExplicitZero(t *testing.T) {
	for _, tt := range []struct {
		name, fields string
		want         int
		explicit     bool
		invalid      bool
	}{
		{name: "default", want: 4},
		{name: "critic default", fields: `,"adversarial":"claude"`, want: 4},
		{name: "one round defaults strict", fields: `,"critic_max_rounds":1`, want: 0},
		{name: "custom maximum", fields: `,"critic_max_rounds":10`, want: 9},
		{name: "zero disables", fields: `,"critic_nonblocking_rounds":0`, explicit: true},
		{name: "first round", fields: `,"critic_nonblocking_rounds":1`, want: 1, explicit: true},
		{name: "last round", fields: `,"critic_max_rounds":3,"critic_nonblocking_rounds":3`, want: 3, explicit: true},
		{name: "null is omitted", fields: `,"critic_nonblocking_rounds":null`, want: 4},
		{name: "negative", fields: `,"critic_nonblocking_rounds":-1`, invalid: true},
		{name: "over hard limit", fields: `,"critic_max_rounds":3,"critic_nonblocking_rounds":4`, invalid: true},
		{name: "fraction", fields: `,"critic_nonblocking_rounds":1.5`, invalid: true},
		{name: "string", fields: `,"critic_nonblocking_rounds":"1"`, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := decode(strings.NewReader(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]` + tt.fields + `}`))
			if err == nil {
				err = cfg.Validate(t.TempDir())
			}
			if tt.invalid {
				if err == nil {
					t.Fatal("accepted invalid nonblocking threshold")
				}
				return
			}
			if err != nil || ResolveCriticNonblockingRounds(cfg.CriticMaxRounds, cfg.CriticNonblockingRounds) != tt.want {
				t.Fatalf("threshold=%v maximum=%d err=%v; want %d", cfg.CriticNonblockingRounds, cfg.CriticMaxRounds, err, tt.want)
			}
			data, err := json.Marshal(cfg)
			if err != nil || strings.Contains(string(data), `"critic_nonblocking_rounds":`) != tt.explicit {
				t.Fatalf("explicit zero/omission lost: %s, %v", data, err)
			}
			var decoded Config
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if ResolveCriticNonblockingRounds(decoded.CriticMaxRounds, decoded.CriticNonblockingRounds) != tt.want {
				t.Fatal("JSON roundtrip changed threshold")
			}
			if !tt.explicit {
				cfg.CriticMaxRounds = 2
				if err := cfg.Validate(t.TempDir()); err != nil || ResolveCriticNonblockingRounds(cfg.CriticMaxRounds, cfg.CriticNonblockingRounds) != 1 {
					t.Fatalf("omitted threshold stopped following maximum: %v", err)
				}
			}
		})
	}
}
