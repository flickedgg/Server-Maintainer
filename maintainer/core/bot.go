package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ServerControl is what the Discord bot can do. *Controller implements it.
type ServerControl interface {
	Status(ctx context.Context) (ServerStatus, error)
	Restart(ctx context.Context, by string) error
	Stop(ctx context.Context, by string) error
	CheckUpdate(ctx context.Context) (UpdateCheckResult, error)
}

// BotCommands are the slash commands the bot registers. They only work inside
// a server (guild), where members have roles.
var BotCommands = []*discordgo.ApplicationCommand{
	{Name: "restart", Description: "Restart the CS2 server (or start it after /stop). Installs a pending update."},
	{Name: "stop", Description: "Stop the CS2 server. It stays down until /restart."},
	{Name: "checkupdate", Description: "Ask Steam for a new CS2 build now."},
	{Name: "health", Description: "Show the CS2 server's status."},
}

func init() {
	guildOnly := []discordgo.InteractionContextType{discordgo.InteractionContextGuild}
	for _, cmd := range BotCommands {
		cmd.Contexts = &guildOnly
	}
}

var errBadBotToken = errors.New("discord rejected the bot token (check discord_bot_token)")

// Bot serves the admin slash commands. Only members with the configured role
// can use them.
type Bot struct {
	token  string
	roleID string
	ctrl   ServerControl
	log    *slog.Logger
}

func NewBot(token, roleID string, ctrl ServerControl, log *slog.Logger) *Bot {
	return &Bot{token: token, roleID: roleID, ctrl: ctrl, log: log}
}

// Run connects to Discord and serves commands until ctx is cancelled. Network
// failures are retried; an invalid token is returned as an error.
func (b *Bot) Run(ctx context.Context) error {
	for attempt := 1; ; attempt++ {
		err := b.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errBadBotToken) {
			return err
		}
		delay := Backoff(attempt, 10*time.Second, 5*time.Minute)
		b.log.Warn("discord bot could not connect; retrying", "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

func (b *Bot) runOnce(ctx context.Context) error {
	s, err := discordgo.New("Bot " + b.token)
	if err != nil {
		return err
	}
	s.Identify.Intents = discordgo.IntentsGuilds // slash commands need no privileged intents
	// Check the token over REST first: a bad token otherwise only shows up as a
	// gateway close code, after which the library keeps reconnecting.
	if _, err := s.User("@me"); err != nil {
		var rest *discordgo.RESTError
		if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusUnauthorized {
			return errBadBotToken
		}
		return err
	}
	s.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		if _, err := s.ApplicationCommandBulkOverwrite(r.Application.ID, "", BotCommands); err != nil {
			b.log.Error("could not register discord slash commands", "err", err)
			return
		}
		b.log.Info("discord bot ready", "as", r.User.Username, "commands", "/restart /stop /checkupdate /health")
	})
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		b.onInteraction(ctx, s, i)
	})
	if err := s.Open(); err != nil {
		return err
	}
	// From here discordgo reconnects by itself.
	<-ctx.Done()
	return s.Close()
}

func (b *Bot) onInteraction(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	name := i.ApplicationCommandData().Name
	if i.Member == nil || i.Member.User == nil {
		b.replyPrivate(s, i, "Use this command in the Discord server, not in DMs.")
		return
	}
	who := "@" + i.Member.User.Username
	if !b.Authorized(i.Member.Roles) {
		b.log.Warn("discord command denied: missing role", "command", name, "user", who)
		b.replyPrivate(s, i, fmt.Sprintf("You need the <@&%s> role to use /%s.", b.roleID, name))
		return
	}
	b.log.Info("discord command", "command", name, "user", who)
	// Acknowledge right away: Discord drops interactions not answered within
	// 3 seconds, and a restart with countdown takes longer.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		b.log.Warn("could not acknowledge discord command", "command", name, "err", err)
		return
	}
	// The interaction token is valid for 15 minutes.
	cctx, cancel := context.WithTimeout(ctx, 14*time.Minute)
	defer cancel()
	text := b.Execute(cctx, name, who)
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:         &text,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	}); err != nil {
		b.log.Warn("could not send discord command result", "command", name, "err", err)
	}
}

func (b *Bot) replyPrivate(s *discordgo.Session, i *discordgo.InteractionCreate, text string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:         text,
			Flags:           discordgo.MessageFlagsEphemeral,
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		},
	})
	if err != nil {
		b.log.Warn("could not reply to discord command", "err", err)
	}
}

// Authorized reports whether a member with these role IDs may use the commands.
func (b *Bot) Authorized(roles []string) bool {
	return b.roleID != "" && slices.Contains(roles, b.roleID)
}

// Execute runs one command for an authorized user and returns the reply text.
func (b *Bot) Execute(ctx context.Context, command, who string) string {
	switch command {
	case "health":
		st, err := b.ctrl.Status(ctx)
		if err != nil {
			return "❌ Couldn't read the server status: " + err.Error()
		}
		return FormatStatus(st)

	case "restart":
		if err := b.ctrl.Restart(ctx, who); err != nil {
			return "❌ Restart failed: " + err.Error()
		}
		msg := "🔄 Server restarted by " + who + "."
		if st, err := b.ctrl.Status(ctx); err == nil {
			msg += "\n" + FormatStatus(st)
		}
		return msg

	case "stop":
		if err := b.ctrl.Stop(ctx, who); err != nil {
			return "❌ Stop failed: " + err.Error()
		}
		return "⏹️ Server stopped by " + who + ". It stays down until someone runs /restart."

	case "checkupdate":
		r, err := b.ctrl.CheckUpdate(ctx)
		switch {
		case err != nil:
			return "❌ Update check failed: " + err.Error()
		case r.Available:
			return fmt.Sprintf("⬆️ Update available: build %s → %s.\n%s", orUnknown(r.Installed), r.Latest, r.Policy)
		default:
			return fmt.Sprintf("✅ Up to date (build %s).", orUnknown(r.Installed))
		}
	}
	return "Unknown command."
}

// FormatStatus renders a ServerStatus as a short Discord message.
func FormatStatus(st ServerStatus) string {
	var lines []string
	switch st.State {
	case StateOnline:
		line := "🟢 **Online**"
		if st.PlayersKnown {
			line += fmt.Sprintf(" · %s · %d/%d players", st.Map, st.Players, st.MaxPlayers)
			if st.Bots > 0 {
				line += fmt.Sprintf(" (+%d bots)", st.Bots)
			}
		}
		lines = append(lines, line+" · up "+HumanDuration(st.Uptime))
	case StateStarting:
		lines = append(lines, "🟡 **Starting** · running for "+HumanDuration(st.Uptime))
	case StateUnresponsive:
		lines = append(lines, fmt.Sprintf("🟠 **Not responding** · %d/%d health checks failed in a row (restarts at %d)",
			st.Failures, st.FailureThreshold, st.FailureThreshold))
	case StateRestarting:
		line := "🟠 **Down, restarting**"
		if st.CrashStreak > 1 {
			line += fmt.Sprintf(" · %d crashes in a row", st.CrashStreak)
		}
		lines = append(lines, line)
	case StateStopped:
		lines = append(lines, "🔴 **Stopped** with /stop · use /restart to start it")
	}

	build := "📦 Build " + orUnknown(st.Installed)
	if st.Pending != "" {
		build += fmt.Sprintf(" · ⬆️ update %s waiting %s. %s", st.Pending, HumanDuration(time.Since(st.PendingSince)), st.UpdatePolicy)
	} else if st.UpdatesEnabled {
		build += " · up to date"
	}
	lines = append(lines, build)

	switch {
	case !st.UpdatesEnabled:
		lines = append(lines, "🔍 Automatic updates are off")
	case st.Checking:
		lines = append(lines, "🔍 Checking for updates now")
	case !st.LastCheck.IsZero():
		lines = append(lines, "🔍 Last update check "+HumanDuration(time.Since(st.LastCheck))+" ago")
	}
	return strings.Join(lines, "\n")
}

// HumanDuration formats d as "45s", "12m" or "4h12m".
func HumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
