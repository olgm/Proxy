package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

// reconcileEvery is how often the other entries are brought level with the
// primary and members who left or lost their role have their lines dropped.
const reconcileEvery = 5 * time.Minute

var commands = []*discordgo.ApplicationCommand{{
	Name:        "whitelist",
	Description: "Manage Minecraft accounts on the proxy whitelist",
	Options: []*discordgo.ApplicationCommandOption{
		{
			Type: discordgo.ApplicationCommandOptionSubCommand, Name: "add",
			Description: "Whitelist a Minecraft account",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "ign", Description: "Minecraft name, or uuid", Required: true},
				{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "Whitelist it for this member instead (managers only)"},
			},
		},
		{
			Type: discordgo.ApplicationCommandOptionSubCommand, Name: "remove",
			Description: "Remove an account you whitelisted",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "player", Description: "Minecraft name, or uuid", Required: true},
			},
		},
		{
			Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list",
			Description: "Show whitelisted accounts",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "Another member's accounts (managers only)"},
			},
		},
		{
			Type: discordgo.ApplicationCommandOptionSubCommand, Name: "purge",
			Description: "Remove every account a member whitelisted (managers only)",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "The member", Required: true},
			},
		},
	},
}, {
	Name:        "watch",
	Description: "Show a member's accounts, open sessions and session history (managers only)",
	Options: []*discordgo.ApplicationCommandOption{
		{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "The member", Required: true},
	},
}}

// noPings renders mentions without notifying anyone: the audit channel is a
// record, and a private reply has one reader already.
var noPings = &discordgo.MessageAllowedMentions{}

func run(b *bot, token string) error {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return err
	}
	// Slash commands arrive without any intent, and the one lookup outside them
	// is a plain REST call. Nothing here needs to see messages or members.
	s.Identify.Intents = discordgo.IntentsNone
	b.members = &guildMembers{s: s, guild: b.cfg.Guild}

	s.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("discord: ready as %s", r.User.Username)
	})
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		b.interaction(s, i)
	})
	if err := s.Open(); err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	defer s.Close()

	// Guild commands take effect at once; global ones can take an hour.
	if _, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, b.cfg.Guild, commands); err != nil {
		return fmt.Errorf("discord: register commands in guild %s: %w", b.cfg.Guild, err)
	}
	log.Printf("discord: /whitelist and /watch registered in guild %s", b.cfg.Guild)

	go func() {
		for {
			for _, line := range b.prune() {
				b.audit(s, line)
			}
			b.chain.reconcile()
			time.Sleep(reconcileEvery)
		}
	}()
	go b.roster.run()
	go b.watcher.run()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return nil
}

func (b *bot) interaction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Member == nil || i.GuildID != b.cfg.Guild {
		return
	}
	if i.Type == discordgo.InteractionMessageComponent {
		b.page(s, i)
		return
	}
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	if data.Name == "watch" {
		b.watchCommand(s, i, data)
		return
	}
	if data.Name != "whitelist" || len(data.Options) == 0 {
		return
	}
	sub := data.Options[0]
	c := command{op: sub.Name}
	for _, o := range sub.Options {
		switch o.Name {
		case "ign", "player":
			c.player = strings.TrimSpace(o.StringValue())
		case "user":
			c.user = o.UserValue(nil).ID
		}
	}

	// Discord wants an answer within three seconds; an add may wait on Mojang
	// and on a far entry for longer than that. Acknowledge, then edit.
	ack := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}
	if err := s.InteractionRespond(i.Interaction, ack); err != nil {
		log.Printf("discord: ack: %v", err)
		return
	}
	r := b.handle(member{id: i.Member.User.ID, roles: i.Member.Roles}, c)
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &r.text, AllowedMentions: noPings}); err != nil {
		log.Printf("discord: reply: %v", err)
	}
	b.audit(s, r.audit)
}

// watchCommand answers /watch with the first page. Ephemeral, always: this is
// the one reply that carries a client IP, and it goes to the one manager who
// asked for it.
func (b *bot) watchCommand(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	user := ""
	for _, o := range data.Options {
		if o.Name == "user" {
			user = o.UserValue(nil).ID
		}
	}
	ack := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}
	if err := s.InteractionRespond(i.Interaction, ack); err != nil {
		log.Printf("discord: ack: %v", err)
		return
	}
	r := b.watch(member{id: i.Member.User.ID, roles: i.Member.Roles}, user, 0)
	rows := pager(r)
	edit := &discordgo.WebhookEdit{Content: &r.text, AllowedMentions: noPings, Components: &rows}
	if _, err := s.InteractionResponseEdit(i.Interaction, edit); err != nil {
		log.Printf("discord: reply: %v", err)
	}
}

// page answers a paging button. The page number came from the button's own id,
// so nothing was remembered between the press and the message; the role is
// checked again because a member may have lost it since the first page.
func (b *bot) page(s *discordgo.Session, i *discordgo.InteractionCreate) {
	user, page, ok := parseWatchID(i.MessageComponentData().CustomID)
	if !ok {
		return
	}
	r := b.watch(member{id: i.Member.User.ID, roles: i.Member.Roles}, user, page)
	resp := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content: r.text, Flags: discordgo.MessageFlagsEphemeral,
			AllowedMentions: noPings, Components: pager(r),
		},
	}
	if err := s.InteractionRespond(i.Interaction, resp); err != nil {
		log.Printf("discord: page: %v", err)
	}
}

// pager is the row of buttons under a watch page, or nothing when there is only
// one page to show.
func pager(r watchResult) []discordgo.MessageComponent {
	var row discordgo.ActionsRow
	if r.page > 0 {
		row.Components = append(row.Components, discordgo.Button{
			Label: "Previous", Style: discordgo.SecondaryButton, CustomID: watchID(r.user, r.page-1),
		})
	}
	if r.more {
		row.Components = append(row.Components, discordgo.Button{
			Label: "Next", Style: discordgo.SecondaryButton, CustomID: watchID(r.user, r.page+1),
		})
	}
	if len(row.Components) == 0 {
		// An empty slice, not nil: nil leaves the buttons from the page before.
		return []discordgo.MessageComponent{}
	}
	return []discordgo.MessageComponent{row}
}

func (b *bot) audit(s *discordgo.Session, line string) {
	if line == "" {
		return
	}
	log.Printf("audit: %s", line)
	if b.cfg.AuditChannel == "" {
		return
	}
	_, err := s.ChannelMessageSendComplex(b.cfg.AuditChannel, &discordgo.MessageSend{Content: line, AllowedMentions: noPings})
	if err != nil {
		log.Printf("discord: audit channel %s: %v", b.cfg.AuditChannel, err)
	}
}

// guildMembers answers prune's question with one REST call per member. Get
// Guild Member needs no privileged intent, unlike listing the members.
type guildMembers struct {
	s     *discordgo.Session
	guild string
}

func (g *guildMembers) lookup(id string) ([]string, bool, error) {
	m, err := g.s.GuildMember(g.guild, id)
	var re *discordgo.RESTError
	if errors.As(err, &re) && re.Message != nil &&
		(re.Message.Code == discordgo.ErrCodeUnknownMember || re.Message.Code == discordgo.ErrCodeUnknownUser) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return m.Roles, true, nil
}
