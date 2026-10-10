package slack

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

var someCommands = []telegram.Command{{Name: "status", Description: "Agents: online or offline"}, {Name: "key", Description: "Press a key: /key 2"}}

func TestTheManifestHasWhatSocketModeAndTheClientNeed(t *testing.T) {
	raw, err := Manifest("petrvs-bridge", "pb-", someCommands)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Features struct {
			Slash []struct {
				Command      string `json:"command"`
				Description  string `json:"description"`
				ShouldEscape bool   `json:"should_escape"`
			} `json:"slash_commands"`
			AppHome struct {
				Messages bool `json:"messages_tab_enabled"`
				ReadOnly bool `json:"messages_tab_read_only_enabled"`
			} `json:"app_home"`
		} `json:"features"`
		OAuth struct {
			Scopes struct {
				Bot []string `json:"bot"`
			} `json:"scopes"`
		} `json:"oauth_config"`
		Settings struct {
			Socket bool `json:"socket_mode_enabled"`
			Events struct {
				Bot []string `json:"bot_events"`
			} `json:"event_subscriptions"`
			Interactivity struct {
				On bool `json:"is_enabled"`
			} `json:"interactivity"`
			Rotation bool `json:"token_rotation_enabled"`
			Org      bool `json:"org_deploy_enabled"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if !m.Settings.Socket || !m.Settings.Interactivity.On || len(m.Settings.Events.Bot) != 1 || m.Settings.Events.Bot[0] != "message.im" || m.Settings.Org {
		t.Fatalf("%+v", m.Settings)
	}
	if !m.Features.AppHome.Messages || m.Features.AppHome.ReadOnly {
		t.Fatal("the user could not write to the app")
	}
	if len(m.Features.Slash) != 2 || m.Features.Slash[0].Command != "/pb-status" || m.Features.Slash[0].ShouldEscape {
		t.Fatalf("%+v", m.Features.Slash)
	}
	want := map[string]bool{"chat:write": true, "commands": true, "files:read": true, "files:write": true, "im:history": true, "im:write": true, "reactions:write": true}
	for _, s := range m.OAuth.Scopes.Bot {
		if !want[s] {
			t.Errorf("a right the client does not use: %s", s)
		}
		delete(want, s)
	}
	if len(want) != 0 {
		t.Errorf("rights missing: %v", want)
	}
	for _, bad := range []string{"channels:", "groups:", "users:", "admin", "search:", "chat:write.public", "chat:write.customize", "usergroups"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("the manifest asks for %s", bad)
		}
	}
}

func TestTheManifestRefusesNamesSlackWouldRefuse(t *testing.T) {
	if _, err := Manifest("x", "Bad Prefix", someCommands); err == nil {
		t.Fatal("a bad prefix")
	}
	if _, err := Manifest("x", "pb-", []telegram.Command{{Name: "UPPER", Description: "x"}}); err == nil {
		t.Fatal("a bad command name")
	}
	if _, err := Manifest("x", "pb-", []telegram.Command{{Name: strings.Repeat("a", 40), Description: "x"}}); err == nil {
		t.Fatal("a command name that is too long")
	}
}

// The file in deploy/slack is what `hub slack-manifest` prints; the test in cmd/hub compares the two. Here only that the file
// is the JSON of a manifest of this client.
func TestTheManifestInTheRepositoryIsValidJSON(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/slack/manifest.json")
	if err != nil {
		t.Skip("no manifest in the repository yet")
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
}

func TestTheDescriptionsNameTheCommandsWithTheirPrefix(t *testing.T) {
	raw, err := Manifest("x", "pb-", []telegram.Command{
		{Name: "alarm", Description: "Alarm status; /alarm on, /alarm off [code]"},
		{Name: "key", Description: "Press a key: /key 2, /key btab; not /keyboard or a/key"},
		{Name: "clear", Description: "Clear the session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/pb-alarm on, /pb-alarm off [code]", "/pb-key 2, /pb-key btab; not /keyboard or a/key"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %q in\n%s", want, raw)
		}
	}
}
