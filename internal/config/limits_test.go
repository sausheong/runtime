package config

import "testing"

func TestResolveLimitsMergeMatrix(t *testing.T) {
	getenv := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	s := func(v string) *string { return &v }
	i := func(v int) *int { return &v }

	cases := []struct {
		name string
		raw  *LimitsConfig
		env  map[string]string
		want Limits
	}{
		{"all absent", nil, nil, Limits{}},
		{"agent sets all", &LimitsConfig{
			TurnTimeout: s("120s"), SessionTimeout: s("30m"),
			MaxTurns: i(50), MaxTokens: i(200000),
		}, nil, Limits{TurnTimeoutMS: 120000, SessionTimeoutMS: 1800000, MaxTurns: 50, MaxTokens: 200000}},
		{"env defaults apply when block absent", nil, map[string]string{
			"RUNTIME_LIMIT_TURN_TIMEOUT":    "60s",
			"RUNTIME_LIMIT_SESSION_TIMEOUT": "10m",
			"RUNTIME_LIMIT_MAX_TURNS":       "10",
			"RUNTIME_LIMIT_MAX_TOKENS":      "5000",
		}, Limits{TurnTimeoutMS: 60000, SessionTimeoutMS: 600000, MaxTurns: 10, MaxTokens: 5000}},
		{"agent field overrides env", &LimitsConfig{MaxTurns: i(99)},
			map[string]string{"RUNTIME_LIMIT_MAX_TURNS": "10"},
			Limits{MaxTurns: 99}},
		{"explicit zero opts out of env default", &LimitsConfig{
			TurnTimeout: s("0s"), MaxTokens: i(0),
		}, map[string]string{
			"RUNTIME_LIMIT_TURN_TIMEOUT": "60s",
			"RUNTIME_LIMIT_MAX_TOKENS":   "5000",
		}, Limits{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveLimits(tc.raw, getenv(tc.env))
			if err != nil {
				t.Fatalf("ResolveLimits: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestResolveLimitsErrors(t *testing.T) {
	s := func(v string) *string { return &v }
	i := func(v int) *int { return &v }
	noenv := func(string) string { return "" }

	for _, tc := range []struct {
		name string
		raw  *LimitsConfig
	}{
		{"bad duration", &LimitsConfig{TurnTimeout: s("banana")}},
		{"negative duration", &LimitsConfig{SessionTimeout: s("-5s")}},
		{"negative max_turns", &LimitsConfig{MaxTurns: i(-1)}},
		{"negative max_tokens", &LimitsConfig{MaxTokens: i(-7)}},
		{"turn timeout exceeds session timeout", &LimitsConfig{
			TurnTimeout: s("10m"), SessionTimeout: s("1m")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveLimits(tc.raw, noenv); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

func TestLimitsJSON(t *testing.T) {
	if got := (Limits{}).JSON(); got != "" {
		t.Errorf("empty Limits JSON = %q, want empty string", got)
	}
	l := Limits{TurnTimeoutMS: 120000, MaxTokens: 200000}
	want := `{"turn_timeout_ms":120000,"max_tokens":200000}`
	if got := l.JSON(); got != want {
		t.Errorf("JSON = %q, want %q", got, want)
	}
}

func TestLoadResolvesLimitsAndAllowsRemote(t *testing.T) {
	// limits: valid on BOTH local and remote agents; resolved on load.
	t.Setenv("RUNTIME_LIMIT_MAX_TURNS", "7")
	p := writeTmp(t, `
agents:
  - id: a1
    name: A1
    model: m
    listen_addr: ":9301"
    limits:
      turn_timeout: 5s
  - id: a2
    name: A2
    model: m
    url: "http://remote:9000"
    limits:
      max_tokens: 100
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents[0].Limits.TurnTimeoutMS != 5000 || cfg.Agents[0].Limits.MaxTurns != 7 {
		t.Errorf("a1 limits = %+v", cfg.Agents[0].Limits)
	}
	if cfg.Agents[1].Limits.MaxTokens != 100 || cfg.Agents[1].Limits.MaxTurns != 7 {
		t.Errorf("a2 limits = %+v", cfg.Agents[1].Limits)
	}
}
