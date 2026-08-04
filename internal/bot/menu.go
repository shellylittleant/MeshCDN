// Bot menu — 5 categories per V4 design (referencing v3.1 alpha9 layout).
//
// Entry: /menu  (or via the /v menu - - command)
//
// Layout:
//
//	📋 MeshCDN v4 Console
//
//	🌐 Domains   🛡 Rules
//	🖥 Nodes     🔀 Routing & Network
//	🤖 AI Assistant
//	📤 Export  🔄 Sync  ⬆️ Upgrade
//	ℹ️ Command Help
//
// Sub-menus accessed via /menu domains, /menu rules, /menu nodes, etc.
//
// All labels and prose come from the i18n catalogue; the command strings the
// buttons emit never do. A button is a shortcut for typing the command, so
// translating it would hand the operator something they cannot retype.
package bot

import (
	"context"
	"strings"

	"github.com/go-telegram/bot/models"

	"github.com/example/meshcdn/internal/command"
)

// showMenu renders a menu page based on the sub-page selector.
//
// "" or "main"  → main menu
// "domains"     → domain management submenu
// "rules"       → rules submenu
// "nodes"       → nodes submenu
// "routing"     → routing & network submenu
// "ai"          → AI assistant submenu
func (c *Client) showMenu(ctx context.Context, original *models.Message, page string) {
	switch strings.TrimSpace(page) {
	case "", "main":
		c.menuMain(ctx, original)
	case "domains":
		c.menuDomains(ctx, original)
	case "rules":
		c.menuRules(ctx, original)
	case "nodes":
		c.menuNodes(ctx, original)
	case "routing":
		c.menuRouting(ctx, original)
	case "ai":
		c.menuAI(ctx, original)
	default:
		c.replyTo(ctx, original, command.T(ctx, "menu.unknown_page", page))
	}
}

// backRow is the "« back to main menu" row shared by every submenu.
func backRow(ctx context.Context) []ButtonData {
	return []ButtonData{{Text: command.T(ctx, "menu.btn.back"), Command: "/menu"}}
}

// ─────────────────────────────────────────────────────────────────────
// Main menu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuMain(ctx context.Context, original *models.Message) {
	text := command.T(ctx, "menu.main.title", c.ProgramVersion) + "\n\n" +
		command.T(ctx, "menu.main.body")

	c.replyWithButtons(ctx, original, text, [][]ButtonData{
		{
			{Text: command.T(ctx, "menu.btn.domains"), Command: "/menu domains"},
			{Text: command.T(ctx, "menu.btn.rules"), Command: "/menu rules"},
		},
		{
			{Text: command.T(ctx, "menu.btn.nodes"), Command: "/menu nodes"},
			{Text: command.T(ctx, "menu.btn.routing"), Command: "/menu routing"},
		},
		{
			{Text: command.T(ctx, "menu.btn.ai"), Command: "/menu ai"},
		},
		{
			{Text: command.T(ctx, "menu.btn.export"), Command: "/v export - -"},
			{Text: command.T(ctx, "menu.btn.sync"), Command: "/v sync - -"},
			{Text: command.T(ctx, "menu.btn.upgrade"), Command: "/v upgrade - -"},
		},
		{
			{Text: command.T(ctx, "menu.btn.help"), Command: "/v help - -"},
		},
	})
}

// ─────────────────────────────────────────────────────────────────────
// 🌐 Domains submenu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuDomains(ctx context.Context, original *models.Message) {
	c.replyWithButtons(ctx, original, command.T(ctx, "menu.domains.body"), [][]ButtonData{
		{
			{Text: command.T(ctx, "menu.domains.btn.list"), Command: "/v domain - -"},
			{Text: command.T(ctx, "menu.domains.btn.certs"), Command: "/v ssl - -"},
		},
		{
			{Text: command.T(ctx, "menu.domains.btn.certhealth"), Command: "/v sslfile - -"},
			{Text: command.T(ctx, "menu.domains.btn.allrules"), Command: "/v export - -"},
		},
		backRow(ctx),
	})
}

// ─────────────────────────────────────────────────────────────────────
// 🛡 Rules submenu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuRules(ctx context.Context, original *models.Message) {
	c.replyWithButtons(ctx, original, command.T(ctx, "menu.rules.body"), [][]ButtonData{
		{
			{Text: command.T(ctx, "menu.rules.btn.cache"), Command: "/v cache - -"},
			{Text: command.T(ctx, "menu.rules.btn.defense"), Command: "/v defense - -"},
		},
		{
			{Text: command.T(ctx, "menu.rules.btn.redirect"), Command: "/v redirect - -"},
			{Text: command.T(ctx, "menu.rules.btn.header"), Command: "/v header - -"},
		},
		{
			{Text: command.T(ctx, "menu.rules.btn.bind"), Command: "/v bind - -"},
		},
		backRow(ctx),
	})
}

// ─────────────────────────────────────────────────────────────────────
// 🖥 Nodes submenu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuNodes(ctx context.Context, original *models.Message) {
	c.replyWithButtons(ctx, original, command.T(ctx, "menu.nodes.body"), [][]ButtonData{
		{
			{Text: command.T(ctx, "menu.nodes.btn.list"), Command: "/v nodes - -"},
			{Text: command.T(ctx, "menu.nodes.btn.status"), Command: "/v status - -"},
		},
		{
			{Text: command.T(ctx, "menu.nodes.btn.stats"), Command: "/v stats - -"},
		},
		{
			{Text: command.T(ctx, "menu.nodes.btn.sync"), Command: "/v sync - -"},
			{Text: command.T(ctx, "menu.nodes.btn.upgrade"), Command: "/v upgrade - -"},
		},
		backRow(ctx),
	})
}

// ─────────────────────────────────────────────────────────────────────
// 🔀 Routing & Network submenu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuRouting(ctx context.Context, original *models.Message) {
	c.replyWithButtons(ctx, original, command.T(ctx, "menu.routing.body"), [][]ButtonData{
		{
			{Text: command.T(ctx, "menu.routing.btn.origins"), Command: "/v domain - -"},
			{Text: command.T(ctx, "menu.routing.btn.peers"), Command: "/v nodes - -"},
		},
		backRow(ctx),
	})
}

// ─────────────────────────────────────────────────────────────────────
// 🤖 AI submenu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuAI(ctx context.Context, original *models.Message) {
	aiStatus := command.T(ctx, "menu.ai.disabled")
	if c.Assistant != nil {
		aiStatus = command.T(ctx, "menu.ai.enabled", c.Assistant.Provider.Name())
	}

	c.replyWithButtons(ctx, original, command.T(ctx, "menu.ai.body", aiStatus), [][]ButtonData{
		{
			{Text: command.T(ctx, "menu.ai.btn.config"), Command: "/v ai - -"},
		},
		backRow(ctx),
	})
}
