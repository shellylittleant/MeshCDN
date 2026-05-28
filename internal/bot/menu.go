// Bot menu — 5 categories per V4 design (referencing v3.1 alpha9 layout).
//
// Entry: /menu  (or via the /v menu - - command)
//
// Layout:
//
//	📋 MeshCDN v4 控制台
//
//	🌐 域名管理   🛡 规则管理
//	🖥 节点管理   🔀 路由&网络
//	🤖 AI 助手
//	📤 导出  🔄 同步  ⬆️ 升级
//	ℹ️ 命令帮助
//
// Sub-menus accessed via /menu domains, /menu rules, /menu nodes, etc.
package bot

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-telegram/bot/models"
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
	page = strings.TrimSpace(page)
	switch page {
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
		c.replyTo(ctx, original, fmt.Sprintf("未知菜单页: %q\n可选: main / domains / rules / nodes / routing / ai", page))
	}
}

// ─────────────────────────────────────────────────────────────────────
// Main menu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuMain(ctx context.Context, original *models.Message) {
	text := fmt.Sprintf(`📋 *MeshCDN %s 控制台*

选择分类，或直接输入命令：
• `+"`/w 类型 scope params`"+` — 写入
• `+"`/d 类型 scope params`"+` — 删除
• `+"`/v 类型 scope params`"+` — 查看
• `+"`/v help - -`"+` — 完整命令参考

也可以 @我 用自然语言提问。`,
		c.ProgramVersion)

	c.replyWithButtons(ctx, original, text, [][]ButtonData{
		{
			{Text: "🌐 域名管理", Command: "/menu domains"},
			{Text: "🛡 规则管理", Command: "/menu rules"},
		},
		{
			{Text: "🖥 节点管理", Command: "/menu nodes"},
			{Text: "🔀 路由&网络", Command: "/menu routing"},
		},
		{
			{Text: "🤖 AI 助手", Command: "/menu ai"},
		},
		{
			{Text: "📤 导出", Command: "/v export - -"},
			{Text: "🔄 同步", Command: "/v sync - -"},
			{Text: "⬆️ 升级", Command: "/v upgrade - -"},
		},
		{
			{Text: "ℹ️ 命令帮助", Command: "/v help - -"},
		},
	})
}

// ─────────────────────────────────────────────────────────────────────
// 🌐 Domains submenu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuDomains(ctx context.Context, original *models.Message) {
	text := `🌐 *域名管理*

域名 = host:port → 源站 的映射关系。
每条域名是独立的，多端口用逗号分隔。

写入示例：
` + "```" + `
/w domain https://a.com:443 https://1.2.3.4:443
/w domain https://a.com:443,8443 https://origin.com:443
/w domain https://-:80 -        (任何 host 落到 80, 无源站=welcome 页)
` + "```"

	c.replyWithButtons(ctx, original, text, [][]ButtonData{
		{
			{Text: "📋 域名列表", Command: "/v domain - -"},
			{Text: "🔒 证书列表", Command: "/v ssl - -"},
		},
		{
			{Text: "🔒 证书健康", Command: "/v sslfile - -"},
			{Text: "📜 全部规则", Command: "/v export - -"},
		},
		{
			{Text: "« 返回主菜单", Command: "/menu"},
		},
	})
}

// ─────────────────────────────────────────────────────────────────────
// 🛡 Rules submenu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuRules(ctx context.Context, original *models.Message) {
	text := `🛡 *规则管理*

V4 用"对象 + 绑定"双层抽象。先定义规则对象，再绑定到具体域名。

` + "```" + `
# 1. 定义对象
/w cache img-7d patterns=*.jpg,*.png ttl=604800 hsts=true
/w defense block-bad ip=1.2.3.4 action=block
/w redirect old-paths from=/old to=/new status=301
/w header secure response_add=X-Frame-Options=DENY

# 2. 绑定到域名
/w bind a.com cache:img-7d
/w bind a.com defense:block-bad
` + "```" + `

同一域名可绑多个规则，nginx 按精度自动排序。`

	c.replyWithButtons(ctx, original, text, [][]ButtonData{
		{
			{Text: "📦 缓存对象", Command: "/v cache - -"},
			{Text: "🛡 防御对象", Command: "/v defense - -"},
		},
		{
			{Text: "↪️ 重定向对象", Command: "/v redirect - -"},
			{Text: "🪪 头部对象", Command: "/v header - -"},
		},
		{
			{Text: "🔗 全部绑定", Command: "/v bind - -"},
		},
		{
			{Text: "« 返回主菜单", Command: "/menu"},
		},
	})
}

// ─────────────────────────────────────────────────────────────────────
// 🖥 Nodes submenu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuNodes(ctx context.Context, original *models.Message) {
	text := `🖥 *节点管理*

集群节点平权，第一个安装的节点默认对接 Telegram。
新节点用 bootstrap.sh 加入集群。`

	c.replyWithButtons(ctx, original, text, [][]ButtonData{
		{
			{Text: "🖥 节点列表", Command: "/v nodes - -"},
			{Text: "📊 本节点状态", Command: "/v status - -"},
		},
		{
			{Text: "📈 流量统计", Command: "/v stats - -"},
		},
		{
			{Text: "🔄 强制同步", Command: "/v sync - -"},
			{Text: "⬆️ 集群升级", Command: "/v upgrade - -"},
		},
		{
			{Text: "« 返回主菜单", Command: "/menu"},
		},
	})
}

// ─────────────────────────────────────────────────────────────────────
// 🔀 Routing & Network submenu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuRouting(ctx context.Context, original *models.Message) {
	text := `🔀 *路由 & 网络*

V4 暂只支持 direct 路径（域名直接到源站）。
后续版本会支持节点中继路径（V4.1）。

端口协议是全集群统一的：每个端口在所有节点用同一个协议（http/https）。
冲突操作会触发 /v confirm 二次确认。`

	c.replyWithButtons(ctx, original, text, [][]ButtonData{
		{
			{Text: "🌐 域名→源站", Command: "/v domain - -"},
			{Text: "🖥 节点连通性", Command: "/v nodes - -"},
		},
		{
			{Text: "« 返回主菜单", Command: "/menu"},
		},
	})
}

// ─────────────────────────────────────────────────────────────────────
// 🤖 AI submenu
// ─────────────────────────────────────────────────────────────────────

func (c *Client) menuAI(ctx context.Context, original *models.Message) {
	aiStatus := "未启用"
	if c.Assistant != nil {
		aiStatus = "✅ 已启用 (provider: " + c.Assistant.Provider.Name() + ")"
	}

	text := fmt.Sprintf(`🤖 *AI 助手*

状态: %s

启用步骤：
`+"```"+`
/w ai key sk-xxx                    # 设 OpenAI API key
/w ai provider openai               # 启用 (默认 model: gpt-4o-mini)
`+"```"+`

使用方式：
- @我 + 自然语言问题 (开启新对话)
- 回复我的消息继续对话 (上下文保留 30 分钟)
- 我会建议命令，你点按钮决定是否执行

支持的 provider：
openai (✅) / gemini (✅) / grok (✅) / claude (✅) / deepseek (✅) / qwen (✅)`,
		aiStatus)

	c.replyWithButtons(ctx, original, text, [][]ButtonData{
		{
			{Text: "🔧 当前配置", Command: "/v ai - -"},
		},
		{
			{Text: "« 返回主菜单", Command: "/menu"},
		},
	})
}
