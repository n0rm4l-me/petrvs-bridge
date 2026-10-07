package screen

import (
	"strings"
	"testing"
)

// A value ends where the shell's syntax starts, so the command that follows a hidden value can still be read. The user
// approves what they see, and the hash covers what is really there.
func TestAHiddenValueEndsAtShellSyntax(t *testing.T) {
	for in, want := range map[string]string{
		`export TOKEN=x;rm${IFS}-rf${IFS}~/;echo done`:  `export TOKEN=<hidden>;rm${IFS}-rf${IFS}~/;echo done`,
		`echo token;rm -rf ~`:                           `echo token;rm -rf ~`,
		`API_KEY=abc|rm -rf ~`:                          `API_KEY=<hidden>|rm -rf ~`,
		`TOKEN=abc&&rm -rf ~`:                           `TOKEN=<hidden>&&rm -rf ~`,
		"TOKEN=abc`rm -rf ~`":                           "TOKEN=<hidden>`rm -rf ~`",
		`TOKEN=abc$(rm -rf ~)`:                          `TOKEN=<hidden>$(rm -rf ~)`,
		`password=abc>out.txt`:                          `password=<hidden>>out.txt`,
		`TOKEN=$(cat f) rm -rf ~`:                       `TOKEN=$(cat f) rm -rf ~`,
		`run --token s3cr3t --flag`:                     `run --token <hidden> --flag`,
		`password="two words here" next`:                `password=<hidden> next`,
		`curl -H "Authorization: Bearer abc123def" url`: `curl -H "Authorization: <hidden> url`,
	} {
		if got := Redact(in); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
}

// The forms in which a secret could get through. Each value must be gone, and what surrounds it must stay.
func TestRedactHidesTheCommonFormsOfSecrets(t *testing.T) {
	for _, c := range []struct{ in, secret string }{
		{`curl -H 'Authorization: token ghp_abcdef' https://example.org`, "ghp_abcdef"},
		{`curl -H "Authorization: Basic dXNlcjpwYXNz" https://example.org`, "dXNlcjpwYXNz"},
		{`curl -H 'Authorization: Bearer abc123def' https://example.org`, "abc123def"},
		{`curl -H 'Cookie: session=abcdef123; other=fedcba321' https://example.org`, "fedcba321"},
		{`curl --cookie 'sid=abcdef123' https://example.org`, "abcdef123"},
		{`redis-cli -u redis://:pw0rd@cache:6379`, "pw0rd"},
		{`git clone https://ghp_abcdefgh@github.com/a/b`, "ghp_abcdefgh"},
		{`git clone https://Zq81xKd3@git.example.org/a/b`, "Zq81xKd3"},
		{`psql postgres://user:p@ss@db.example.org/x`, "ss@db"},
		{`psql postgres://user:hunter2@db.example.org/x`, "hunter2"},
		{`curl -u admin:hunter2 https://example.org`, "hunter2"},
		{`curl --user admin:hunter2 https://example.org`, "hunter2"},
		{`curl -u 'admin:hunter2' https://example.org`, "hunter2"},
		{`mysql -h db -uroot -phunter2 mydb`, "hunter2"},
		{`sshpass -p hunter2 ssh host`, "hunter2"},
		{`sshpass -p'hunter2' ssh host`, "hunter2"},
		{`docker login -u me -p hunter2 registry.example.org`, "hunter2"},
		{`{"token": "hunter2", "n": 1}`, "hunter2"},
		{`{"api_key":"hunter2"}`, "hunter2"},
		{`DB_PASS=hunter2 ./run`, "hunter2"},
		{`pass=hunter2 ./run`, "hunter2"},
		{`PASSWORD=hunter2 ./run`, "hunter2"},
		{`pwd=hunter2 ./run`, "hunter2"},
		{`STRIPE_KEY=sk_live_abcdef123456 ./run`, "sk_live_abcdef123456"},
		{`SSH_KEY=hunter2 ./run`, "hunter2"},
		{`GITHUB_PAT=ghp_abcdef ./run`, "ghp_abcdef"},
		{`passphrase=hunter2 ./run`, "hunter2"},
		{`gpg --passphrase hunter2 -d f`, "hunter2"},
		{`private_key=hunter2 ./run`, "hunter2"},
		{`"private_key": "hunter2"`, "hunter2"},
		{`openssl enc -aes256 -k hunter2 -in a`, "hunter2"},
		{`openssl enc -aes256 -pass pass:hunter2 -in a`, "hunter2"},
		{`SESSION_ID=abcdef123 ./run`, "abcdef123"},
		{`aws_credentials: hunter2`, "hunter2"},
		{"password:\nhunter2\nnext", "hunter2"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAyOuR\nk2fGjz0bQ==\n-----END RSA PRIVATE KEY-----", "MIIEowIBAAKCAQEAyOuR"},
		{"cat <<EOF\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\nk2fGjz0bQ==", "b3BlbnNzaC1rZXk"},
		{`echo xoxb-123456-abcdefgh`, "xoxb-123456-abcdefgh"},
		{`echo sk-ant-api03-abcdefghijklmnop`, "sk-ant-api03-abcdefghijklmnop"},
		{`echo AKIAABCDEFGHIJKLMNOP`, "AKIAABCDEFGHIJKLMNOP"},
		{`echo eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJl`, "c2lnbmF0dXJl"},
		{`echo github_pat_11ABCDEFG0abcdefghij_klmn`, "github_pat_11ABCDEFG0abcdefghij_klmn"},
	} {
		got := Redact(c.in)
		if strings.Contains(got, c.secret) || !strings.Contains(got, "<hidden>") {
			t.Errorf("%q\n  gives %q", c.in, got)
		}
		if again := Redact(got); again != got {
			t.Errorf("hiding twice is not the same as once:\n%q\n%q", got, again)
		}
	}
}

// What is hidden too much makes a dialog unreadable, and a user who cannot read it taps blindly. Ordinary commands stay.
func TestRedactLeavesOrdinaryCommandsAlone(t *testing.T) {
	for _, in := range []string{
		`mkdir -p /tmp/x`,
		`ssh -p 22 host`,
		`docker run -u 1000:1000 -p 8080:80 image`,
		`mysql -p mydb`,
		`git clone ssh://git@github.com/a/b`,
		`git clone https://github.com/a/b`,
		`curl 'https://example.org/?mail=a@b.c'`,
		`echo task-force sk-short`,
		`Bypass permissions mode: passed: 5 failed: 0`,
		`cd /usr/bin && ls -la path=/x pattern: y`,
		`echo "the passport and the compass"`,
		`--- PASS: TestSomething (0.00s)`,
		`ok  	petrvs-bridge/internal/screen	0.5s`,
		`pwd; echo pass`,
		`sudo -u deploy -p x`,
		`tar -xzpf a.tgz`,
		`ls ~/.ssh/id_ed25519.pub`,
		`echo <<EOF`,
	} {
		if got := Redact(in); got != in {
			t.Errorf("%q\n  became %q", in, got)
		}
	}
}

func TestRedactKeepsPlainUserNamesInUrls(t *testing.T) {
	for in, want := range map[string]string{
		`git clone ssh://git@github.com/a/b`:          `git clone ssh://git@github.com/a/b`,
		`git clone https://deploy@git.example.org/a`:  `git clone https://deploy@git.example.org/a`,
		`git clone https://Deploy9@git.example.org/a`: `git clone https://<hidden>@git.example.org/a`,
		`git clone https://:@git.example.org/a`:       `git clone https://:<hidden>@git.example.org/a`,
	} {
		if got := Redact(in); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestBottomLinesUseTheSameRules(t *testing.T) {
	got := BottomLines(screenOf("$ curl -u a:hunter2 https://x", "$ export TOKEN=abc;ls"), 5)
	if strings.Contains(got, "hunter2") || strings.Contains(got, "abc;") || !strings.Contains(got, ";ls") {
		t.Errorf("%q", got)
	}
}
