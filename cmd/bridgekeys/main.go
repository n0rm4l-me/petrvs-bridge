// Command bridgekeys creates and inspects identities and one-time-code secrets.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"claudebridge/internal/approval"
	"claudebridge/internal/identity"
)

const usage = `usage:
  bridgekeys gen -name NAME [-out DIR]     create NAME.identity.json (private, 0600) and NAME.pub.json (share it)
  bridgekeys fingerprint FILE              print the fingerprint of a .pub.json or .identity.json file
  bridgekeys totp-secret -out FILE         create a one-time-code secret (0600) and print how to add it to an authenticator app
  bridgekeys totp-code FILE                print the current code for a secret file (for testing)
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "gen":
		return gen(args[1:])
	case "fingerprint":
		return fingerprint(args[1:])
	case "totp-secret":
		return totpSecret(args[1:])
	case "totp-code":
		return totpCode(args[1:])
	}
	fmt.Fprint(os.Stderr, usage)
	return 2
}

func fail(err error) int { fmt.Fprintln(os.Stderr, "error:", err); return 1 }

func gen(args []string) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	name := fs.String("name", "", "party name: lower-case letters, digits, - and _")
	out := fs.String("out", ".", "directory for the files")
	if fs.Parse(args) != nil {
		return 2
	}
	id, err := identity.Generate(*name)
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return fail(err)
	}
	priv, pub := filepath.Join(*out, *name+".identity.json"), filepath.Join(*out, *name+".pub.json")
	for _, p := range []string{priv, pub} {
		if _, err := os.Stat(p); err == nil {
			return fail(fmt.Errorf("%s already exists; refusing to overwrite", p))
		}
	}
	if err := id.Save(priv); err != nil {
		return fail(err)
	}
	if err := identity.SavePublic(pub, id.Public()); err != nil {
		return fail(err)
	}
	fmt.Printf("private identity: %s (keep it on this machine only)\npublic bundle:    %s (copy this to the other side)\nfingerprint:      %s\n", priv, pub, id.Public().Fingerprint())
	fmt.Println("Compare the fingerprint over a second channel when you copy the public bundle.")
	return 0
}

func fingerprint(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if id, err := identity.Load(args[0]); err == nil {
		fmt.Println(id.Public().Fingerprint())
		return 0
	}
	p, err := identity.LoadPublic(args[0])
	if err != nil {
		return fail(err)
	}
	fmt.Println(p.Fingerprint())
	return 0
}

func totpSecret(args []string) int {
	fs := flag.NewFlagSet("totp-secret", flag.ContinueOnError)
	out := fs.String("out", "", "file to create (0600)")
	if fs.Parse(args) != nil || *out == "" {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	_, b32, err := approval.GenerateSecret()
	if err != nil {
		return fail(err)
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, b32); err != nil {
		return fail(err)
	}
	fmt.Printf("secret written to %s (0600).\nAdd it to an authenticator app, preferably on a different device from your chat app:\n  otpauth://totp/claude-bridge:alarm?secret=%s&issuer=claude-bridge\nThen set CB_ALARM_SECRET_FILE=%s on the agent.\n", *out, b32, *out)
	return 0
}

func totpCode(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		return fail(err)
	}
	s, err := approval.DecodeSecret(string(b))
	if err != nil {
		return fail(err)
	}
	fmt.Println(approval.Code(s, timeNow()))
	return 0
}
