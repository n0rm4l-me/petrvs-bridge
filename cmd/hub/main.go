// Command hub is the server: it talks to Telegram and relays to the agents over Pub/Sub.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/config"
	"github.com/n0rm4l-me/petrvs-bridge/internal/health"
	"github.com/n0rm4l-me/petrvs-bridge/internal/hubsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/slack"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func main() { os.Exit(run(os.Args[1:], os.Getenv)) }

func level(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func run(args []string, getenv func(string) string) int {
	if len(args) > 0 && args[0] == "probe" {
		return probe(args[1:], getenv, time.Now(), os.Stderr)
	}
	if len(args) > 0 && args[0] == "slack-manifest" {
		return slackManifest(args[1:], os.Stdout, os.Stderr)
	}
	cfg, err := config.ParseHub(args, getenv)
	if errors.Is(err, flag.ErrHelp) {
		return 0 // the usage has been printed on standard output
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	log := newLogger(os.Stdout, cfg.LogLevel)
	id, err := identity.LoadOpts(cfg.Identity, cfg.AllowGroupRead)
	if err != nil {
		log.Error("cannot load the hub identity", "err", err)
		return 1
	}
	files, _ := filepath.Glob(filepath.Join(cfg.AgentsDir, "*.pub.json"))
	var agents []identity.Public
	for _, f := range files {
		p, err := identity.LoadPublic(f)
		if err != nil {
			log.Error("cannot load an agent bundle", "file", f, "err", err)
			return 1
		}
		if p.Name == id.Name {
			log.Error("an agent has the hub's name", "name", p.Name)
			return 1
		}
		agents = append(agents, p)
	}
	if len(agents) == 0 {
		log.Error("no agents are pinned", "dir", cfg.AgentsDir)
		return 1
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	log.Info("hub identity", "name", id.Name, "fingerprint", id.Public().Fingerprint())
	for _, a := range agents {
		log.Info("pinned agent", "name", a.Name, "fingerprint", a.Fingerprint())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal the hub drains (see hubsvc.Run); a second one ends it at once, as it would without this handler.
	go func() { <-ctx.Done(); stop() }()
	tr, err := transport.NewPubSub(ctx, cfg.Project)
	if err != nil {
		log.Error("cannot connect to Pub/Sub", "err", err)
		return 1
	}
	defer tr.Close()
	tr.SetPullMode(transport.PullMode(cfg.Pull))
	names := wire.Names{Prefix: cfg.Prefix}
	if cfg.CreateResources {
		if err := ensure(ctx, tr, names, agents); err != nil {
			log.Error("cannot create topics", "err", err)
			return 1
		}
	}
	var beacon hubsvc.Health
	if cfg.HealthDir != "" {
		b, err := health.NewBeacon(cfg.HealthDir)
		if err != nil {
			log.Error("cannot use the health directory", "dir", cfg.HealthDir, "err", err)
			return 1
		}
		beacon = b
	}
	chat, helpNote, slackDone, err := openChat(ctx, cfg, log)
	if err != nil {
		log.Error("cannot set up the chat", "err", err)
		return 1
	}
	hub, err := hubsvc.New(hubsvc.Config{
		Identity: id, Agents: agents, Transport: tr, Names: names, HelpNote: helpNote, ChatName: chatName(cfg),
		Telegram: chat, ChatID: cfg.ChatID, UserID: cfg.UserID, Log: log,
		OfflineAfter: cfg.OfflineAfter, TextTTL: cfg.TextTTL, KeyTTL: cfg.KeyTTL, PollTimeout: cfg.PollTimeout, MaxReplyBytes: cfg.MaxReplyBytes,
		LockFile: cfg.LockFile, ReceivedReaction: cfg.ReceivedReaction, DeliveredReaction: cfg.DeliveredReaction, FailedReaction: cfg.FailedReaction, DefaultAgent: cfg.DefaultAgent,
		MaxFileBytes: cfg.MaxFileBytes, Health: beacon, ShutdownTimeout: cfg.ShutdownTimeout,
	})
	if err != nil {
		log.Error("invalid hub configuration", "err", err)
		return 1
	}
	log.Info("hub running", "agents", len(agents), "chat", cfg.Chat)
	err = hub.Run(ctx)
	stop()
	slackDone() // the Slack connection ends with the hub, so that the process does not wait on it
	if err != nil {
		log.Error("hub stopped", "err", err)
		return 1
	}
	log.Info("hub stopped")
	return 0
}

// openChat makes the client of the chat service the hub talks to, and the note the hub adds to its list of commands. For Slack
// it also starts the connection (Socket Mode), which lives until the returned function is called or ctx ends.
func openChat(ctx context.Context, cfg *config.Hub, log *slog.Logger) (telegram.Client, string, func(), error) {
	if cfg.Chat != "slack" {
		return telegram.NewHTTP(cfg.Token).WithBaseURL(cfg.TelegramBase), "", func() {}, nil
	}
	sl, err := slack.New(slack.Config{
		BotToken: cfg.SlackBotToken, AppToken: cfg.SlackAppToken, UserID: cfg.SlackUserID, ChannelID: cfg.SlackChannelID,
		APIBase: cfg.SlackAPIBase, CommandPrefix: cfg.SlackCommandPrefix, Log: log,
	})
	if err != nil {
		return nil, "", nil, err
	}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); sl.Run(rctx) }()
	stopSlack := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	return sl, slack.HelpNote(cfg.SlackCommandPrefix), stopSlack, nil
}

func ensure(ctx context.Context, tr *transport.PubSub, names wire.Names, agents []identity.Public) error {
	if err := tr.EnsureTopic(ctx, names.ToHub()); err != nil {
		return err
	}
	if err := tr.EnsureSubscription(ctx, names.ToHub(), names.HubSub(), 30*time.Second); err != nil {
		return err
	}
	for _, a := range agents {
		if err := tr.EnsureTopic(ctx, names.ToAgent(a.Name)); err != nil {
			return err
		}
		if err := tr.EnsureSubscription(ctx, names.ToAgent(a.Name), names.AgentSub(a.Name), 30*time.Second); err != nil {
			return err
		}
	}
	return nil
}

// chatName is how the hub names its chat service to the user.
func chatName(cfg *config.Hub) string {
	if cfg.Chat == "slack" {
		return "Slack"
	}
	return "Telegram"
}
