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
	"github.com/n0rm4l-me/petrvs-bridge/internal/hubsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
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
	tr, err := transport.NewPubSub(ctx, cfg.Project)
	if err != nil {
		log.Error("cannot connect to Pub/Sub", "err", err)
		return 1
	}
	defer tr.Close()
	names := wire.Names{Prefix: cfg.Prefix}
	if cfg.CreateResources {
		if err := ensure(ctx, tr, names, agents); err != nil {
			log.Error("cannot create topics", "err", err)
			return 1
		}
	}
	hub, err := hubsvc.New(hubsvc.Config{
		Identity: id, Agents: agents, Transport: tr, Names: names,
		Telegram: telegram.NewHTTP(cfg.Token).WithBaseURL(cfg.TelegramBase), ChatID: cfg.ChatID, UserID: cfg.UserID, Log: log,
		OfflineAfter: cfg.OfflineAfter, TextTTL: cfg.TextTTL, KeyTTL: cfg.KeyTTL, PollTimeout: cfg.PollTimeout, MaxReplyBytes: cfg.MaxReplyBytes,
		LockFile: cfg.LockFile, ReceivedReaction: cfg.ReceivedReaction, DeliveredReaction: cfg.DeliveredReaction, DefaultAgent: cfg.DefaultAgent,
		MaxFileBytes: cfg.MaxFileBytes,
	})
	if err != nil {
		log.Error("invalid hub configuration", "err", err)
		return 1
	}
	log.Info("hub running", "agents", len(agents))
	if err := hub.Run(ctx); err != nil {
		log.Error("hub stopped", "err", err)
		return 1
	}
	log.Info("hub stopped")
	return 0
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
