package i18n

// Message catalogue.
//
// Keys are dotted and grouped by surface: menu.*, help.*, status.*, nodes.*,
// stats.*, lang.*, err.*. Both tables must carry the same key set; a test
// asserts that, so a half-translated release cannot ship.
//
// Command syntax inside these strings is deliberately NOT translated — see the
// package doc.

var zh = map[string]string{
	// ── Language switching ──────────────────────────────────────────
	"lang.switched":   "✅ 界面语言已切换为 %s，已同步到全集群。",
	"lang.unchanged":  "界面语言已经是 %s，无变化。",
	"lang.current":    "当前界面语言: %s\n切换: /cn (中文) 或 /en (English)",
	"lang.unknown":    "无法识别的语言 %q。可选: cn (中文) / en (English)",
	"lang.usage":      "用法: /v lang <cn|en> -   或直接发 /cn 、/en",
	"lang.hint_after": "命令语法不随语言改变 —— /w /d /v 始终一致。",

	// ── Main menu ───────────────────────────────────────────────────
	"menu.main.title": "📋 *MeshCDN %s 控制台*",
	"menu.main.body": `选择分类，或直接输入命令：
• ` + "`/w 类型 scope params`" + ` — 写入
• ` + "`/d 类型 scope params`" + ` — 删除
• ` + "`/v 类型 scope params`" + ` — 查看
• ` + "`/v help - -`" + ` — 完整命令参考

也可以 @我 用自然语言提问。`,
	"menu.btn.domains":  "🌐 域名管理",
	"menu.btn.rules":    "🛡 规则管理",
	"menu.btn.nodes":    "🖥 节点管理",
	"menu.btn.routing":  "🔀 路由&网络",
	"menu.btn.ai":       "🤖 AI 助手",
	"menu.btn.export":   "📤 导出",
	"menu.btn.sync":     "🔄 同步",
	"menu.btn.upgrade":  "⬆️ 升级",
	"menu.btn.help":     "ℹ️ 命令帮助",
	"menu.btn.back":     "« 返回主菜单",
	"menu.unknown_page": "未知菜单页: %q\n可选: main / domains / rules / nodes / routing / ai",

	// ── Domains submenu ─────────────────────────────────────────────
	"menu.domains.body": `🌐 *域名管理*

域名 = host:port → 源站 的映射关系。
每条域名是独立的，多端口用逗号分隔。

写入示例：
` + "```" + `
/w domain https://a.com:443 https://1.2.3.4:443
/w domain https://a.com:443,8443 https://origin.com:443
/w domain https://-:80 -        (任何 host 落到 80, 无源站=welcome 页)
` + "```",
	"menu.domains.btn.list":       "📋 域名列表",
	"menu.domains.btn.certs":      "🔒 证书列表",
	"menu.domains.btn.certhealth": "🔒 证书健康",
	"menu.domains.btn.allrules":   "📜 全部规则",

	// ── Rules submenu ───────────────────────────────────────────────
	"menu.rules.body": `🛡 *规则管理*

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

同一域名可绑多个规则，nginx 按精度自动排序。`,
	"menu.rules.btn.cache":    "📦 缓存对象",
	"menu.rules.btn.defense":  "🛡 防御对象",
	"menu.rules.btn.redirect": "↪️ 重定向对象",
	"menu.rules.btn.header":   "🪪 头部对象",
	"menu.rules.btn.bind":     "🔗 全部绑定",

	// ── Nodes submenu ───────────────────────────────────────────────
	"menu.nodes.body": `🖥 *节点管理*

集群节点平权，第一个安装的节点默认对接 Telegram。
新节点用 quick-install.sh --peer=<任一在线节点IP> 加入集群。`,
	"menu.nodes.btn.list":    "🖥 节点列表",
	"menu.nodes.btn.status":  "📊 本节点状态",
	"menu.nodes.btn.stats":   "📈 流量统计",
	"menu.nodes.btn.sync":    "🔄 强制同步",
	"menu.nodes.btn.upgrade": "⬆️ 集群升级",

	// ── Routing submenu ─────────────────────────────────────────────
	"menu.routing.body": `🔀 *路由 & 网络*

V4 暂只支持 direct 路径（域名直接到源站）。
后续版本会支持节点中继路径（V4.1）。

端口协议是全集群统一的：每个端口在所有节点用同一个协议（http/https）。
冲突操作会触发 /v confirm 二次确认。`,
	"menu.routing.btn.origins": "🌐 域名→源站",
	"menu.routing.btn.peers":   "🖥 节点连通性",

	// ── AI submenu ──────────────────────────────────────────────────
	"menu.ai.body": `🤖 *AI 助手*

状态: %s

启用步骤：
` + "```" + `
/w ai key sk-xxx                    # 设 OpenAI API key
/w ai provider openai               # 启用 (默认 model: gpt-4o-mini)
` + "```" + `

使用方式：
- @我 + 自然语言问题 (开启新对话)
- 回复我的消息继续对话 (上下文保留 30 分钟)
- 我会建议命令，你点按钮决定是否执行

支持的 provider：
openai (✅) / gemini (✅) / grok (✅) / claude (✅) / deepseek (✅) / qwen (✅)`,
	"menu.ai.disabled":   "未启用",
	"menu.ai.enabled":    "✅ 已启用 (provider: %s)",
	"menu.ai.btn.config": "🔧 当前配置",

	// ── Status ──────────────────────────────────────────────────────
	"status.title":      "MeshCDN 节点状态",
	"status.node_ip":    "节点 IP",
	"status.bot_node":   "bot 节点",
	"status.program":    "程序版本",
	"status.config_ver": "config_version",
	"status.peer_count": "集群节点数",
	"status.domains":    "域名规则",
	"status.rule_objs":  "规则对象",
	"status.bindings":   "绑定关系",
	"status.language":   "界面语言",
	"status.now":        "当前时间",
	"status.unknown":    "(未知)",
	"status.this_node":  "(本节点)",

	// ── Nodes ───────────────────────────────────────────────────────
	"nodes.title":   "集群节点 (%d)  ⭐=bot:",
	"nodes.online":  "online",
	"nodes.offline": "offline",
	"nodes.no_data": "(local or no data)",

	// ── Stats ───────────────────────────────────────────────────────
	"stats.disabled":    "stats: 未启用 (本节点未运行日志采集，需 serve 模式 + nginx)",
	"stats.title":       "📊 流量统计 (%s, 近 %s, 本节点)",
	"stats.all_domains": "全部域名",
	"stats.no_data":     "  (窗口内无数据)",
	"stats.total_hits":  "  总请求: %d",
	"stats.total_bytes": "  总流量: %s",
	"stats.by_status":   "  状态码分布:",
	"stats.top_domains": "  域名 Top:",
	"stats.hits_unit":   "次",

	// ── Batch report ────────────────────────────────────────────────
	"report.all_ok":      "✅ %d 条命令成功执行 (config_version: %d → %d)",
	"report.partial":     "⚠️  %d 条成功, %d 条失败 (config_version: %d → %d)",
	"report.all_failed":  "❌ %d 条命令全部失败 (config_version 未变: %d)",
	"report.empty":       "(空批处理)",
	"report.line_failed": "  L%d %s: %v",

	// ── Export ──────────────────────────────────────────────────────
	"export.caption": "📤 配置 v%d — %d 条命令",

	// ── Misc handlers ───────────────────────────────────────────────
	"confirm.done":  "确认完成: %s",
	"menu.cli_hint": "💡 在 Telegram 群里发 /menu (不带占位符) 显示带按钮的主菜单。\nCLI 模式下查看完整命令: /v help - -",

	// ── Error code descriptions ─────────────────────────────────────
	"err.BAD_FORMAT":       "命令格式错误",
	"err.UNKNOWN_TYPE":     "未知的命令类型",
	"err.BAD_PARAMS":       "参数错误",
	"err.NOT_FOUND":        "未找到",
	"err.CONFIRM_REQUIRED": "需要二次确认",
	"err.CONFIRM_EXPIRED":  "确认已过期",
	"err.CONFIRM_UNKNOWN":  "确认 ID 无效",
	"err.PORT_CONFLICT":    "端口协议冲突",
	"err.CASCADE_BLOCKED":  "对象仍被引用，无法删除",
	"err.INTERNAL":         "内部错误",
	"err.exec_failed":      "❌ 执行失败: %v",
	"err.file_send_failed": "❌ 文件发送失败: %v",

	// ── Help ────────────────────────────────────────────────────────
	"help.text": `MeshCDN 命令参考

格式: /<verb> <type> <scope> <params>   (严格四段式，空位用 - 占位)

A 类 - 直接命令
  /w domain   <host:port>       <origin>             写入域名
  /w ssl      <域名/IP>          -                    申请 LE 证书
  /w sslfile  <域名/IP>          -                    上传证书 (env vars)
  /d <type>   <scope>           <params>             删除 (与 /w 镜像)

B 类 - 对象命令
  /w cache    <名字>            <key=value...>      定义缓存对象
  /v cache    -                 purge-all           清空全集群缓存并 reload
  /v cache    -                 purge-node          仅清空本节点缓存
  /w defense  <名字>            <key=value...>      定义防御对象
  /w redirect <名字>            <key=value...>      定义重定向对象
  /w header   <名字>            <key=value...>      定义头部对象

C 类 - 绑定命令
  /w bind     <域名/IP>          <对象类型>:<对象名>

V 类 - 查询命令
  /v <type>   <scope|->         <params|->          查询规则
  /v export   -                 -                    导出全集群配置
  /v status   -                 -                    本节点状态
  /v nodes    [<peer-ip>|-]     -                    peer 列表/详情
  /v stats    [<域名>|-]        [<窗口>|-]           流量统计 (本节点; 窗口如 24h/7d, 默认 24h)

系统动作 (read-shaped, 用 /v 调用)
  /v sync     -                 -                    强制同步给所有 peer
  /v target   <peer-ip>         -                    转移 bot 角色 (重启后生效)
  /v upgrade  -                 -                    触发集群升级
  /v lang     <cn|en>           -                    切换界面语言 (全集群)
  /v help     -                 -                    本帮助
  /v menu     -                 -                    主菜单 (Telegram 用)
  /v confirm  <ID>              -                    二次确认 (危险操作)

Telegram 简写: /menu /help /sync /en /cn

详细文档: V4-DESIGN.md`,
}

var en = map[string]string{
	// ── Language switching ──────────────────────────────────────────
	"lang.switched":   "✅ Interface language set to %s, synced across the cluster.",
	"lang.unchanged":  "Interface language is already %s; nothing changed.",
	"lang.current":    "Current interface language: %s\nSwitch with /en (English) or /cn (中文)",
	"lang.unknown":    "Unrecognised language %q. Choose: cn (中文) / en (English)",
	"lang.usage":      "Usage: /v lang <cn|en> -   or just send /en or /cn",
	"lang.hint_after": "Command syntax does not change with language — /w /d /v are always the same.",

	// ── Main menu ───────────────────────────────────────────────────
	"menu.main.title": "📋 *MeshCDN %s Console*",
	"menu.main.body": `Pick a category, or type a command directly:
• ` + "`/w <type> <scope> <params>`" + ` — write
• ` + "`/d <type> <scope> <params>`" + ` — delete
• ` + "`/v <type> <scope> <params>`" + ` — view
• ` + "`/v help - -`" + ` — full command reference

You can also @mention me and ask in plain language.`,
	"menu.btn.domains":  "🌐 Domains",
	"menu.btn.rules":    "🛡 Rules",
	"menu.btn.nodes":    "🖥 Nodes",
	"menu.btn.routing":  "🔀 Routing & Network",
	"menu.btn.ai":       "🤖 AI Assistant",
	"menu.btn.export":   "📤 Export",
	"menu.btn.sync":     "🔄 Sync",
	"menu.btn.upgrade":  "⬆️ Upgrade",
	"menu.btn.help":     "ℹ️ Command Help",
	"menu.btn.back":     "« Back to main menu",
	"menu.unknown_page": "Unknown menu page: %q\nAvailable: main / domains / rules / nodes / routing / ai",

	// ── Domains submenu ─────────────────────────────────────────────
	"menu.domains.body": `🌐 *Domains*

A domain is a host:port → origin mapping.
Each entry is independent; list multiple ports comma-separated.

Examples:
` + "```" + `
/w domain https://a.com:443 https://1.2.3.4:443
/w domain https://a.com:443,8443 https://origin.com:443
/w domain https://-:80 -        (any host on :80, no origin = welcome page)
` + "```",
	"menu.domains.btn.list":       "📋 Domain list",
	"menu.domains.btn.certs":      "🔒 Certificates",
	"menu.domains.btn.certhealth": "🔒 Cert health",
	"menu.domains.btn.allrules":   "📜 All rules",

	// ── Rules submenu ───────────────────────────────────────────────
	"menu.rules.body": `🛡 *Rules*

V4 uses an object + binding model. Define a rule object once, then bind it
to as many domains as you like.

` + "```" + `
# 1. define the objects
/w cache img-7d patterns=*.jpg,*.png ttl=604800 hsts=true
/w defense block-bad ip=1.2.3.4 action=block
/w redirect old-paths from=/old to=/new status=301
/w header secure response_add=X-Frame-Options=DENY

# 2. bind them to a domain
/w bind a.com cache:img-7d
/w bind a.com defense:block-bad
` + "```" + `

One domain can carry many rules; nginx orders them by precision automatically.`,
	"menu.rules.btn.cache":    "📦 Cache objects",
	"menu.rules.btn.defense":  "🛡 Defense objects",
	"menu.rules.btn.redirect": "↪️ Redirect objects",
	"menu.rules.btn.header":   "🪪 Header objects",
	"menu.rules.btn.bind":     "🔗 All bindings",

	// ── Nodes submenu ───────────────────────────────────────────────
	"menu.nodes.body": `🖥 *Nodes*

All nodes are equals; the first one installed talks to Telegram by default.
Add a node with quick-install.sh --peer=<any live node IP>.`,
	"menu.nodes.btn.list":    "🖥 Node list",
	"menu.nodes.btn.status":  "📊 This node",
	"menu.nodes.btn.stats":   "📈 Traffic stats",
	"menu.nodes.btn.sync":    "🔄 Force sync",
	"menu.nodes.btn.upgrade": "⬆️ Cluster upgrade",

	// ── Routing submenu ─────────────────────────────────────────────
	"menu.routing.body": `🔀 *Routing & Network*

V4 supports direct paths only (domain straight to origin).
Relaying through peers is planned for V4.1.

Port protocol is cluster-wide: a given port uses one protocol (http/https)
on every node. Contradicting that triggers a /v confirm prompt.`,
	"menu.routing.btn.origins": "🌐 Domain → origin",
	"menu.routing.btn.peers":   "🖥 Peer reachability",

	// ── AI submenu ──────────────────────────────────────────────────
	"menu.ai.body": `🤖 *AI Assistant*

Status: %s

To enable:
` + "```" + `
/w ai key sk-xxx                    # set the OpenAI API key
/w ai provider openai               # enable (default model: gpt-4o-mini)
` + "```" + `

How to use:
- @mention me with a question (starts a new conversation)
- Reply to my message to continue it (context kept for 30 minutes)
- I suggest commands; you press a button to decide whether to run them

Supported providers:
openai (✅) / gemini (✅) / grok (✅) / claude (✅) / deepseek (✅) / qwen (✅)`,
	"menu.ai.disabled":   "not enabled",
	"menu.ai.enabled":    "✅ enabled (provider: %s)",
	"menu.ai.btn.config": "🔧 Current config",

	// ── Status ──────────────────────────────────────────────────────
	"status.title":      "MeshCDN node status",
	"status.node_ip":    "Node IP",
	"status.bot_node":   "Bot node",
	"status.program":    "Program version",
	"status.config_ver": "config_version",
	"status.peer_count": "Cluster nodes",
	"status.domains":    "Domain rules",
	"status.rule_objs":  "Rule objects",
	"status.bindings":   "Bindings",
	"status.language":   "Interface language",
	"status.now":        "Current time",
	"status.unknown":    "(unknown)",
	"status.this_node":  "(this node)",

	// ── Nodes ───────────────────────────────────────────────────────
	"nodes.title":   "Cluster nodes (%d)  ⭐=bot:",
	"nodes.online":  "online",
	"nodes.offline": "offline",
	"nodes.no_data": "(local or no data)",

	// ── Stats ───────────────────────────────────────────────────────
	"stats.disabled":    "stats: unavailable (log collection is not running here — needs serve mode + nginx)",
	"stats.title":       "📊 Traffic (%s, last %s, this node)",
	"stats.all_domains": "all domains",
	"stats.no_data":     "  (no data in window)",
	"stats.total_hits":  "  Requests: %d",
	"stats.total_bytes": "  Bytes:    %s",
	"stats.by_status":   "  By status code:",
	"stats.top_domains": "  Top domains:",
	"stats.hits_unit":   "hits",

	// ── Batch report ────────────────────────────────────────────────
	"report.all_ok":      "✅ %d command(s) executed (config_version: %d → %d)",
	"report.partial":     "⚠️  %d succeeded, %d failed (config_version: %d → %d)",
	"report.all_failed":  "❌ all %d command(s) failed (config_version unchanged: %d)",
	"report.empty":       "(empty batch)",
	"report.line_failed": "  L%d %s: %v",

	// ── Export ──────────────────────────────────────────────────────
	"export.caption": "📤 Config v%d — %d commands",

	// ── Misc handlers ───────────────────────────────────────────────
	"confirm.done":  "Confirmed: %s",
	"menu.cli_hint": "💡 Send /menu (no placeholders) in the Telegram group for the button menu.\nFrom the CLI, see the full command list with: /v help - -",

	// ── Error code descriptions ─────────────────────────────────────
	"err.BAD_FORMAT":       "malformed command",
	"err.UNKNOWN_TYPE":     "unknown command type",
	"err.BAD_PARAMS":       "invalid parameters",
	"err.NOT_FOUND":        "not found",
	"err.CONFIRM_REQUIRED": "confirmation required",
	"err.CONFIRM_EXPIRED":  "confirmation expired",
	"err.CONFIRM_UNKNOWN":  "unknown confirmation ID",
	"err.PORT_CONFLICT":    "port protocol conflict",
	"err.CASCADE_BLOCKED":  "object is still referenced and cannot be deleted",
	"err.INTERNAL":         "internal error",
	"err.exec_failed":      "❌ execution failed: %v",
	"err.file_send_failed": "❌ could not send file: %v",

	// ── Help ────────────────────────────────────────────────────────
	"help.text": `MeshCDN command reference

Form: /<verb> <type> <scope> <params>   (always 4 segments; use - for an empty slot)

Class A — direct commands
  /w domain   <host:port>       <origin>            register a domain
  /w ssl      <domain/IP>       -                   request a Let's Encrypt cert
  /w sslfile  <domain/IP>       -                   upload a cert (env vars)
  /d <type>   <scope>           <params>            delete (mirrors /w)

Class B — rule objects
  /w cache    <name>            <key=value...>      define a cache object
  /v cache    -                 purge-all           purge every node's cache + reload
  /v cache    -                 purge-node          purge this node's cache only
  /w defense  <name>            <key=value...>      define a defense object
  /w redirect <name>            <key=value...>      define a redirect object
  /w header   <name>            <key=value...>      define a header object

Class C — bindings
  /w bind     <domain/IP>       <objtype>:<objname>

Queries
  /v <type>   <scope|->         <params|->          inspect rules
  /v export   -                 -                   export the whole config
  /v status   -                 -                   this node's status
  /v nodes    [<peer-ip>|-]     -                   peer list / detail
  /v stats    [<domain>|-]      [<window>|-]        traffic (this node; e.g. 24h/7d, default 24h)

System actions (read-shaped, invoked with /v)
  /v sync     -                 -                   push a sync to every peer
  /v target   <peer-ip>         -                   move the bot role (takes effect on restart)
  /v upgrade  -                 -                   trigger a cluster upgrade
  /v lang     <cn|en>           -                   switch interface language (cluster-wide)
  /v help     -                 -                   this help
  /v menu     -                 -                   main menu (Telegram)
  /v confirm  <ID>              -                   confirm a held dangerous action

Telegram shorthands: /menu /help /sync /en /cn

Full documentation: V4-DESIGN.md`,
}
