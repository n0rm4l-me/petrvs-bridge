package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// Action says what the worker does with a request of the assistant to use a tool.
type Action int

const (
	// Ask puts the request to the chat and waits for an answer.
	Ask Action = iota
	// Allow lets the assistant use the tool without asking.
	Allow
	// Deny refuses the request without asking the chat.
	Deny
)

// Decision is the verdict of a Policy on one request.
type Decision struct {
	Action Action
	// Reason says why a request was allowed or denied.
	Reason string
	// Risk, for an Ask, names what is risky about the request; the dialog in the chat shows it.
	Risk string
}

// PolicyFile is the JSON form of a policy. A list that is absent keeps the default; a list that is present, even an empty
// one, replaces it.
type PolicyFile struct {
	// AllowTools are tools that run without asking (Read, Glob, ...). Bash is never allowed by this list.
	AllowTools []string `json:"allow_tools"`
	// AllowBash are regular expressions for Bash commands that run without asking, unless they are risky. Anchor them:
	// "^git status$" and not "git status", which also matches "rm x; git status".
	AllowBash []string `json:"allow_bash"`
	// DenyBash are regular expressions for Bash commands that are refused without asking the chat.
	DenyBash []string `json:"deny_bash"`
	// RiskyBash are regular expressions for Bash commands that always go to the chat, flagged, even when AllowBash matches.
	RiskyBash []string `json:"risky_bash"`
	// RiskyPaths are regular expressions for the paths that the file tools (Read, Write, Edit, Glob, Grep, ...) touch and that are flagged.
	RiskyPaths []string `json:"risky_paths"`
}

type riskRule struct {
	re    *regexp.Regexp
	label string
}

// Policy decides which requests of the assistant need the chat. The assistant itself already runs harmless commands (reading
// files, `echo`) without asking; a request reaches the policy only when it would prompt. The order is: deny, then risky (always
// asked, flagged), then allow, then ask.
type Policy struct {
	allowTools map[string]bool
	allowBash  []*regexp.Regexp
	denyBash   []*regexp.Regexp
	riskyBash  []riskRule
	riskyPaths []riskRule
}

// The defaults: read-only tools and the helper tools of the assistant run without asking, nothing else does; a few commands that
// cannot be meant are refused; commands that change infrastructure or secrets are flagged in the dialog.
var (
	// Reads inside the working directory never reach the policy: the assistant allows them itself. A read that does reach it is
	// outside that directory, and is a question for the chat, so Read, Glob and Grep are not in the default list.
	defaultAllowTools = []string{"TodoWrite", "Task"}
	defaultDenyBash   = []string{
		`(^|[;&|]\s*)rm\s+(-[a-zA-Z]*\s+)*-[a-zA-Z]*[rR][a-zA-Z]*\s+(-[a-zA-Z]+\s+)*/\*?\s*($|[;&|])`,
		`(^|[;&|]\s*)mkfs(\.\w+)?\s`,
		`(^|[;&|]\s*)dd\s+.*\bof=/dev/`,
		`:\(\)\s*\{\s*:\|:&\s*\}\s*;\s*:`,
	}
	defaultRiskyBash = []riskRule{
		{regexp.MustCompile(`\bkubectl\b.*\b(apply|delete|patch|scale|replace|annotate|label|edit|rollout|drain|cordon|create|set)\b`), "changes a Kubernetes cluster"},
		{regexp.MustCompile(`\bhelm\b.*\b(install|upgrade|uninstall|rollback)\b`), "changes a Helm release"},
		{regexp.MustCompile(`\bterraform\b.*\b(apply|destroy|import|state\s+(rm|mv))\b`), "changes infrastructure (Terraform)"},
		{regexp.MustCompile(`\bargocd\b.*\b(sync|delete|rollback)\b`), "syncs or changes an ArgoCD application"},
		{regexp.MustCompile(`\bgh\s+pr\s+merge\b`), "merges a pull request"},
		{regexp.MustCompile(`\bgit\s+push\b`), "pushes to a remote repository"},
		{regexp.MustCompile(`\bgcloud\b.*\b(delete|destroy|set-iam-policy|add-iam-policy-binding|remove-iam-policy-binding)\b`), "deletes or changes cloud resources or access"},
		{regexp.MustCompile(`\b(shutdown|reboot|halt|poweroff)\b`), "powers the machine off"},
	}
	defaultRiskyPaths = []riskRule{
		{regexp.MustCompile(`(^|/)\.(env|ssh|aws|kube|gnupg)(/|$|\.)`), "touches a secrets or credentials path"},
		{regexp.MustCompile(`(?i)(\.pem|\.key|\.p12|\.pfx|id_rsa|id_ed25519|credentials[^/]*)$`), "touches a key or credentials file"},
		{regexp.MustCompile(`(^|/)[^/]*\.identity\.json$`), "touches an identity (private key) file of the bridge"},
		{regexp.MustCompile(`(^|/)petrvs-bridge/(keys|cloud-auth|handoff)(/|$)`), "touches the keys or credentials of the bridge"},
	}
)

// DefaultPolicy is the policy a worker has when it is given none.
func DefaultPolicy() *Policy {
	p, err := newPolicy(PolicyFile{})
	if err != nil {
		panic(err) // the defaults are fixed text; they compile or the program is wrong
	}
	return p
}

// LoadPolicy reads a policy from a JSON file. Unknown fields are refused, so that a typo does not silently leave the default.
func LoadPolicy(path string) (*Policy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f PolicyFile
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("worker policy %s: %w", path, err)
	}
	return newPolicy(f)
}

func newPolicy(f PolicyFile) (*Policy, error) {
	p := &Policy{allowTools: map[string]bool{}}
	tools := defaultAllowTools
	if f.AllowTools != nil {
		tools = f.AllowTools
	}
	for _, t := range tools {
		if t == "Bash" {
			return nil, fmt.Errorf("worker policy: Bash cannot be in allow_tools; use allow_bash with anchored patterns")
		}
		p.allowTools[t] = true
	}
	var err error
	if p.allowBash, err = compileAll("allow_bash", f.AllowBash); err != nil {
		return nil, err
	}
	deny := defaultDenyBash
	if f.DenyBash != nil {
		deny = f.DenyBash
	}
	if p.denyBash, err = compileAll("deny_bash", deny); err != nil {
		return nil, err
	}
	p.riskyBash = defaultRiskyBash
	if f.RiskyBash != nil {
		p.riskyBash = nil
		rs, err := compileAll("risky_bash", f.RiskyBash)
		if err != nil {
			return nil, err
		}
		for _, re := range rs {
			p.riskyBash = append(p.riskyBash, riskRule{re, "risky command (policy)"})
		}
	}
	p.riskyPaths = defaultRiskyPaths
	if f.RiskyPaths != nil {
		p.riskyPaths = nil
		rs, err := compileAll("risky_paths", f.RiskyPaths)
		if err != nil {
			return nil, err
		}
		for _, re := range rs {
			p.riskyPaths = append(p.riskyPaths, riskRule{re, "risky path (policy)"})
		}
	}
	return p, nil
}

func compileAll(field string, patterns []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, s := range patterns {
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("worker policy: %s: %q: %w", field, s, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// Decide gives the verdict on one request. input is the tool's input as the assistant sent it.
func (p *Policy) Decide(tool string, input map[string]any) Decision {
	switch tool {
	case "Bash":
		cmd, _ := input["command"].(string)
		for _, re := range p.denyBash {
			if re.MatchString(cmd) {
				return Decision{Action: Deny, Reason: "the command matches a deny pattern of the policy"}
			}
		}
		for _, r := range p.riskyBash {
			if r.re.MatchString(cmd) {
				return Decision{Action: Ask, Risk: r.label}
			}
		}
		for _, re := range p.allowBash {
			if re.MatchString(cmd) {
				return Decision{Action: Allow, Reason: "the command matches an allow pattern of the policy"}
			}
		}
		return Decision{Action: Ask}
	case "Write", "Edit", "MultiEdit", "NotebookEdit", "Read", "NotebookRead", "Glob", "Grep", "LS":
		// Reading a secret is as much a risk as writing one: what is read lands in the conversation.
		for _, key := range []string{"file_path", "notebook_path", "path"} {
			path, _ := input[key].(string)
			if path == "" {
				continue
			}
			for _, r := range p.riskyPaths {
				if r.re.MatchString(path) {
					return Decision{Action: Ask, Risk: r.label}
				}
			}
		}
	}
	if tool != "Bash" && p.allowTools[tool] {
		return Decision{Action: Allow, Reason: "the tool is read-only or a helper"}
	}
	return Decision{Action: Ask}
}
