package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSlackManifestPrintsTheManifestOfTheRepository(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := slackManifest(nil, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	// deploy/slack/manifest.json is this output with the defaults; if the hub's commands change, regenerate it:
	//   go run ./cmd/hub slack-manifest > deploy/slack/manifest.json
	want, err := os.ReadFile("../../deploy/slack/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(out.String()) {
		t.Fatal("deploy/slack/manifest.json is not what `hub slack-manifest` prints; regenerate it with: go run ./cmd/hub slack-manifest > deploy/slack/manifest.json")
	}
	if !strings.Contains(out.String(), `"/pb-lockdown"`) || !strings.Contains(out.String(), `"/pb-key"`) {
		t.Fatal("the commands of the hub are missing")
	}
}

func TestSlackManifestTakesAPrefixAndAName(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := slackManifest([]string{"-name", "my bridge", "-prefix", "x_"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"/x_status"`) || !strings.Contains(out.String(), `"my bridge"`) || strings.Contains(out.String(), "/pb-") {
		t.Fatal(out.String())
	}
	for _, args := range [][]string{{"-prefix", "Bad Prefix"}, {"-name", ""}, {"-name", strings.Repeat("n", 40)}, {"extra"}, {"-unknown"}} {
		out.Reset()
		errOut.Reset()
		if code := slackManifest(args, &out, &errOut); code != 2 || out.Len() != 0 {
			t.Errorf("%v: exit %d, output %q", args, code, out.String())
		}
	}
}
