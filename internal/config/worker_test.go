package config

import (
	"strings"
	"testing"
	"time"
)

func workerEnv(t *testing.T, extra map[string]string) map[string]string {
	e := agentEnv(t)
	e["PB_WORKER"] = "true"
	e["PB_WORKDIR"] = "/work/project"
	for k, v := range extra {
		e[k] = v
	}
	return e
}

func TestWorkerModeIsOffByDefault(t *testing.T) {
	a, err := ParseAgent(nil, envOf(agentEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if a.Worker.Enabled {
		t.Fatalf("worker mode is on without being asked: %+v", a.Worker)
	}
}

func TestWorkerDefaults(t *testing.T) {
	a, err := ParseAgent(nil, envOf(workerEnv(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	w := a.Worker
	if !w.Enabled || w.Command != "claude" || w.Model != "" || w.BudgetUSD != 5 || w.SettingSources != "project,local" ||
		w.TurnTimeout != 30*time.Minute || w.ApprovalTimeout != 30*time.Minute || w.PromptFile != "" || w.PolicyFile != "" {
		t.Fatalf("%+v", w)
	}
}

func TestWorkerSettingsFromTheEnvironmentAndFlags(t *testing.T) {
	e := workerEnv(t, map[string]string{
		"PB_WORKER_COMMAND": "/opt/bin/claude", "PB_WORKER_MODEL": "sonnet", "PB_WORKER_BUDGET_USD": "1.25",
		"PB_WORKER_PROMPT_FILE": "/etc/pb/prompt.md", "PB_WORKER_POLICY_FILE": "/etc/pb/policy.json",
		"PB_WORKER_SETTING_SOURCES": "user,project", "PB_WORKER_TURN_TIMEOUT": "10m", "PB_WORKER_APPROVAL_TIMEOUT": "2m",
	})
	a, err := ParseAgent(nil, envOf(e))
	if err != nil {
		t.Fatal(err)
	}
	w := a.Worker
	if w.Command != "/opt/bin/claude" || w.Model != "sonnet" || w.BudgetUSD != 1.25 || w.PromptFile != "/etc/pb/prompt.md" ||
		w.PolicyFile != "/etc/pb/policy.json" || w.SettingSources != "user,project" || w.TurnTimeout != 10*time.Minute || w.ApprovalTimeout != 2*time.Minute {
		t.Fatalf("%+v", w)
	}
	// A flag beats the environment.
	a, err = ParseAgent([]string{"-worker-model", "haiku", "-worker-budget-usd", "-1"}, envOf(e))
	if err != nil {
		t.Fatal(err)
	}
	if a.Worker.Model != "haiku" || a.Worker.BudgetUSD != -1 {
		t.Fatalf("%+v", a.Worker)
	}
}

func TestTheTurnLimitCanBeSwitchedOff(t *testing.T) {
	for _, v := range []string{"0", "off", "OFF"} {
		a, err := ParseAgent(nil, envOf(workerEnv(t, map[string]string{"PB_WORKER_TURN_TIMEOUT": v})))
		if err != nil || a.Worker.TurnTimeout != 0 {
			t.Errorf("%q: %v %v", v, a.Worker.TurnTimeout, err)
		}
	}
}

func TestWorkerSettingsThatAreWrongAreRefused(t *testing.T) {
	for name, c := range map[string]struct {
		extra map[string]string
		want  string
	}{
		"no workdir":        {map[string]string{"PB_WORKDIR": ""}, "PB_WORKDIR"},
		"heal with worker":  {map[string]string{"PB_HEAL": "true"}, "PB_HEAL"},
		"relative prompt":   {map[string]string{"PB_WORKER_PROMPT_FILE": "prompt.md"}, "worker-prompt-file"},
		"relative policy":   {map[string]string{"PB_WORKER_POLICY_FILE": "p.json"}, "worker-policy-file"},
		"budget not number": {map[string]string{"PB_WORKER_BUDGET_USD": "plenty"}, "PB_WORKER_BUDGET_USD"},
		"command two lines": {map[string]string{"PB_WORKER_COMMAND": "claude\nrm -rf /"}, "worker-command"},
		"bad turn limit":    {map[string]string{"PB_WORKER_TURN_TIMEOUT": "soon"}, "PB_WORKER_TURN_TIMEOUT"},
	} {
		_, err := ParseAgent(nil, envOf(workerEnv(t, c.extra)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestWorkerSettingsAreNotCheckedWhenWorkerModeIsOff(t *testing.T) {
	e := agentEnv(t)
	e["PB_WORKER_PROMPT_FILE"] = "relative.md" // would be refused in worker mode; there is no worker
	if _, err := ParseAgent(nil, envOf(e)); err != nil {
		t.Fatal(err)
	}
}
