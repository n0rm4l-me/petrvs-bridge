package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheDefaultPolicy(t *testing.T) {
	p := DefaultPolicy()
	bash := func(cmd string) map[string]any { return map[string]any{"command": cmd} }
	for _, c := range []struct {
		name  string
		tool  string
		input map[string]any
		want  Action
		risk  string
	}{
		{"a read outside the workdir asks", "Read", map[string]any{"file_path": "/etc/hosts"}, Ask, ""},
		{"a search outside the workdir asks", "Grep", map[string]any{"path": "/var/log"}, Ask, ""},
		{"reading ssh keys is flagged", "Read", map[string]any{"file_path": "/home/me/.ssh/id_ed25519"}, Ask, "touches a secrets or credentials path"},
		{"reading an identity file is flagged", "Read", map[string]any{"file_path": "/home/me/.config/petrvs-bridge/keys/mac.identity.json"}, Ask, "touches an identity (private key) file of the bridge"},
		{"listing the bridge credentials is flagged", "LS", map[string]any{"path": "/home/me/.config/petrvs-bridge/cloud-auth"}, Ask, "touches the keys or credentials of the bridge"},
		{"a search of aws config is flagged", "Grep", map[string]any{"path": "/home/me/.aws"}, Ask, "touches a secrets or credentials path"},
		{"the todo list", "TodoWrite", nil, Allow, ""},
		{"a subagent", "Task", nil, Allow, ""},
		{"an unknown tool asks", "Mystery", nil, Ask, ""},
		{"web fetch asks", "WebFetch", nil, Ask, ""},
		{"a plain command asks", "Bash", bash("go test ./..."), Ask, ""},
		{"kubectl apply is flagged", "Bash", bash("kubectl --context=c -n n apply -f x.yaml"), Ask, "changes a Kubernetes cluster"},
		{"kubectl get is not", "Bash", bash("kubectl --context=c -n n get pods"), Ask, ""},
		{"terraform apply is flagged", "Bash", bash("cd x && terraform apply plan.out"), Ask, "changes infrastructure (Terraform)"},
		{"terraform plan is not", "Bash", bash("terraform plan -out=p"), Ask, ""},
		{"merge is flagged", "Bash", bash("gh pr merge 5 --squash --admin"), Ask, "merges a pull request"},
		{"push is flagged", "Bash", bash("git push origin main"), Ask, "pushes to a remote repository"},
		{"argocd sync is flagged", "Bash", bash("argocd app sync x"), Ask, "syncs or changes an ArgoCD application"},
		{"gcloud delete is flagged", "Bash", bash("gcloud pubsub topics delete t --project p"), Ask, "deletes or changes cloud resources or access"},
		{"helm upgrade is flagged", "Bash", bash("helm upgrade r c -f v.yaml"), Ask, "changes a Helm release"},
		{"shutdown is flagged", "Bash", bash("sudo shutdown -h now"), Ask, "powers the machine off"},
		{"rm -rf / is refused", "Bash", bash("rm -rf /"), Deny, ""},
		{"rm -rf /* is refused", "Bash", bash("echo hi; rm -fr /*"), Deny, ""},
		{"rm of a directory is not", "Bash", bash("rm -rf ./build"), Ask, ""},
		{"mkfs is refused", "Bash", bash("mkfs.ext4 /dev/sda1"), Deny, ""},
		{"dd to a device is refused", "Bash", bash("dd if=/dev/zero of=/dev/disk2"), Deny, ""},
		{"a fork bomb is refused", "Bash", bash(":(){ :|:& };:"), Deny, ""},
		{"writing .env is flagged", "Write", map[string]any{"file_path": "/home/me/app/.env"}, Ask, "touches a secrets or credentials path"},
		{"editing a key is flagged", "Edit", map[string]any{"file_path": "/home/me/id_ed25519"}, Ask, "touches a key or credentials file"},
		{"editing code asks", "Edit", map[string]any{"file_path": "/home/me/app/main.go"}, Ask, ""},
	} {
		d := p.Decide(c.tool, c.input)
		if d.Action != c.want || d.Risk != c.risk {
			t.Errorf("%s: %+v, want action %d risk %q", c.name, d, c.want, c.risk)
		}
	}
}

func TestAPolicyFileReplacesOnlyTheListsItHas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	body := `{"allow_bash": ["^git status$", "^go (build|vet|test)( |$)"], "allow_tools": ["Read"]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	bash := func(cmd string) map[string]any { return map[string]any{"command": cmd} }
	if d := p.Decide("Bash", bash("git status")); d.Action != Allow {
		t.Errorf("git status: %+v", d)
	}
	if d := p.Decide("Bash", bash("go test ./...")); d.Action != Allow {
		t.Errorf("go test: %+v", d)
	}
	if d := p.Decide("Bash", bash("rm -rf x; git status")); d.Action != Ask {
		t.Errorf("an anchored pattern let a chained command through: %+v", d)
	}
	// A risky command is asked about even when an allow pattern matches it.
	p2, _ := newPolicy(PolicyFile{AllowBash: []string{`kubectl`}})
	if d := p2.Decide("Bash", bash("kubectl delete pod x")); d.Action != Ask || d.Risk == "" {
		t.Errorf("an allow pattern overrode the risk flag: %+v", d)
	}
	// The lists that the file does not have keep the defaults; the one it has replaces them.
	if d := p.Decide("Bash", bash("rm -rf /")); d.Action != Deny {
		t.Errorf("the default deny list was lost: %+v", d)
	}
	if d := p.Decide("TodoWrite", nil); d.Action != Ask {
		t.Errorf("allow_tools did not replace the default: %+v", d)
	}
	if d := p.Decide("Read", map[string]any{"file_path": "/home/me/.ssh/id_rsa"}); d.Action != Ask || d.Risk == "" {
		t.Errorf("a tool in allow_tools reads a secret without a flag: %+v", d)
	}
}

func TestAPolicyFileThatIsWrongIsRefused(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"typo":         `{"allow_bsh": ["x"]}`,
		"bad regex":    `{"deny_bash": ["("]}`,
		"bash allowed": `{"allow_tools": ["Read", "Bash"]}`,
		"not json":     `allow everything`,
	} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPolicy(path); err == nil {
			t.Errorf("%s: a wrong policy was accepted", name)
		}
	}
	if _, err := LoadPolicy(filepath.Join(dir, "missing.json")); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("missing file: %v", err)
	}
}
