package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/n0rm4l-me/petrvs-bridge/internal/hubsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/slack"
)

// slackManifest prints the manifest of the Slack app for this hub: `hub slack-manifest [-name NAME] [-prefix PREFIX]`. Make the
// app from it (api.slack.com/apps, Create New App, From a manifest). It needs no settings and no network.
func slackManifest(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("slack-manifest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "petrvs-bridge", "the name of the app and of its bot")
	prefix := fs.String("prefix", "pb-", "the prefix of the slash commands; it must be the hub's PB_SLACK_COMMAND_PREFIX")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: hub slack-manifest [-name NAME] [-prefix PREFIX]")
		return 2
	}
	if n := len([]rune(*name)); n == 0 || n > 35 {
		fmt.Fprintln(stderr, "slack-manifest: the name is 1 to 35 characters")
		return 2
	}
	out, err := slack.Manifest(*name, *prefix, hubsvc.ChatCommands())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	fmt.Fprintf(stdout, "%s\n", out)
	return 0
}
