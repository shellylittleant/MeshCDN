// Package bot implements the Telegram bot interface — UPDATED for step 8.
//
// Changes from step 7:
//
//   - 5-category menu (参考 v3.1):
//     🌐 域名管理   🛡 规则管理
//     🖥 节点管理   🔀 路由&网络
//     🤖 AI 助手
//     📤 导出  🔄 同步  ⬆️ 升级
//     ℹ️ 命令帮助
//
//   - Inline keyboard buttons send their command text back as message
//
//   - @mention triggers AI conversation; replies to bot continue conversation
//
//   - AI suggestions (\`\`\`command blocks) presented with execute/cancel buttons
package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/example/meshcdn/internal/ai"
	"github.com/example/meshcdn/internal/command"
	"github.com/example/meshcdn/internal/identity"
)

// Client wraps the Telegram bot.
type Client struct {
	Token   string
	GroupID int64

	Executor *command.Executor

	// AI assistant. May be nil (AI disabled).
	Assistant *ai.Assistant

	// PendingStore tracks /v confirm IDs.
	PendingStore PendingStore

	// FileFetcher for cert uploads.
	FileFetcher FileFetcher

	LocalNodeIP    string
	ProgramVersion string

	// uploadBuffers for multi-file cert uploads
	uploadBuffers uploadBuffers

	// botUsername is "@meshcdnbot" (filled in via getMe at startup)
	botUsername string

	// pendingExec stores AI-suggested commands waiting for user confirmation
	pendingExec   map[string]string // execID → command text
	pendingExecMu sync.Mutex

	bot   *tgbot.Bot
	mu    sync.Mutex
	ready bool
}

type PendingStore interface {
	Add(id, cmdText string)
	Resolve(id string) (string, error)
}

type FileFetcher interface {
	Fetch(ctx context.Context, fileID string) ([]byte, error)
}

// Start launches the bot's long-polling loop.
func (c *Client) Start(ctx context.Context) error {
	if c.Token == "" {
		return errors.New("bot token is empty")
	}
	if c.GroupID == 0 {
		return errors.New("group_id is 0")
	}
	if c.Executor == nil {
		return errors.New("executor not configured")
	}

	c.pendingExec = make(map[string]string)

	opts := []tgbot.Option{
		tgbot.WithDefaultHandler(c.defaultHandler),
	}

	b, err := tgbot.New(c.Token, opts...)
	if err != nil {
		return fmt.Errorf("init bot: %w", err)
	}

	c.mu.Lock()
	c.bot = b
	c.ready = true
	c.mu.Unlock()

	// Probe own username so we can detect @mentions
	go func() {
		// Brief wait for bot to be fully initialized
		time.Sleep(500 * time.Millisecond)
		me, err := b.GetMe(ctx)
		if err == nil && me != nil {
			c.mu.Lock()
			c.botUsername = "@" + me.Username
			c.mu.Unlock()
			log.Printf("[bot] username: %s", c.botUsername)
		}
	}()

	log.Printf("[bot] starting long-polling for group %d", c.GroupID)

	go func() {
		time.Sleep(1 * time.Second)
		c.SendToGroup(ctx, "🟢 MeshCDN agent online (bot ready)")
	}()

	b.Start(ctx)
	log.Printf("[bot] long-polling stopped")
	return nil
}

// WireFileFetcherWhenReady asynchronously sets up the FileFetcher.
func (c *Client) WireFileFetcherWhenReady(ctx context.Context, token string) {
	go func() {
		for i := 0; i < 30; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			c.mu.Lock()
			b := c.bot
			c.mu.Unlock()
			if b != nil {
				c.FileFetcher = &TelegramFileFetcher{Bot: b, Token: token}
				return
			}
		}
		log.Printf("[bot] FileFetcher wiring timeout (file uploads disabled)")
	}()
}

// SendToGroup posts a plain message to the group.
func (c *Client) SendToGroup(ctx context.Context, text string) {
	c.mu.Lock()
	b := c.bot
	ready := c.ready
	c.mu.Unlock()

	if !ready || b == nil {
		log.Printf("[bot] (offline) %s", text)
		return
	}

	for _, chunk := range chunkText(text, 4000) {
		_, err := b.SendMessage(ctx, &tgbot.SendMessageParams{
			ChatID: c.GroupID,
			Text:   chunk,
		})
		if err != nil {
			log.Printf("[bot] send: %v", err)
			return
		}
	}
}

// defaultHandler routes incoming updates.
func (c *Client) defaultHandler(ctx context.Context, b *tgbot.Bot, update *models.Update) {
	// Inline button callback
	if update.CallbackQuery != nil {
		c.handleCallback(ctx, b, update.CallbackQuery)
		return
	}

	if update.Message == nil {
		return
	}
	msg := update.Message

	if msg.Chat.ID != c.GroupID {
		return
	}

	if msg.Document != nil {
		c.handleDocument(ctx, b, msg)
		return
	}

	if msg.Text == "" {
		return
	}

	// Detect @mention or reply-to-bot
	mentionedMe, isReplyToMe := c.detectAITrigger(msg)

	text := strings.TrimSpace(msg.Text)
	textNoMention := stripBotMention(text)

	// Slash command path (/...): always commands, never AI
	if strings.HasPrefix(textNoMention, "/") {
		c.handleTextCommand(ctx, b, msg, textNoMention)
		return
	}

	// Otherwise: only react if @mentioned or replying to a bot message
	if mentionedMe {
		// Fresh conversation
		c.handleAIStart(ctx, msg, textNoMention)
		return
	}
	if isReplyToMe && c.Assistant != nil {
		userID := userIDOf(msg)
		if c.Assistant.HasActiveConv(userID) {
			c.handleAIContinue(ctx, msg, textNoMention)
			return
		}
		// Reply-to-bot but no active conversation; ignore silently to avoid noise
		return
	}
	// Neither command nor AI trigger — silent
}

// detectAITrigger inspects the message to see if AI should engage.
func (c *Client) detectAITrigger(msg *models.Message) (mentioned, isReplyToMe bool) {
	c.mu.Lock()
	username := c.botUsername
	c.mu.Unlock()

	if username != "" && strings.Contains(msg.Text, username) {
		mentioned = true
	}
	// MentionEntity check (more robust than substring)
	for _, e := range msg.Entities {
		if e.Type == "mention" {
			text := msg.Text[e.Offset : e.Offset+e.Length]
			if username != "" && text == username {
				mentioned = true
				break
			}
		}
	}
	// Reply-to-bot
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		// Heuristic: if the replied-to message was sent by us, count as continuation
		if msg.ReplyToMessage.From.IsBot && username != "" {
			// We can't easily get our own user ID without GetMe; assume any bot
			// reply is ours within our group.
			isReplyToMe = true
		}
	}
	return
}

// handleTextCommand processes a /-prefixed command.
func (c *Client) handleTextCommand(ctx context.Context, b *tgbot.Bot, msg *models.Message, text string) {
	// Special: /menu shortcuts
	if strings.HasPrefix(text, "/menu") {
		rest := strings.TrimSpace(strings.TrimPrefix(text, "/menu"))
		c.showMenu(ctx, msg, rest)
		return
	}

	result, err := c.Executor.ExecuteBatch(ctx, text)
	if err != nil {
		c.replyTo(ctx, msg, fmt.Sprintf("❌ 执行失败: %v", err))
		return
	}

	// V4.0.19: if any command produced a file (currently only /v export),
	// deliver as a Telegram document with the merged UserMessage as caption.
	// Skip the standard text report to avoid duplication.
	if att := result.AggregatedEffects.FileAttachment; att != nil {
		caption := strings.TrimSpace(result.AggregatedEffects.UserMessage)
		if caption == "" {
			caption = "📤 " + att.Filename
		}
		if err := c.replyWithFile(ctx, msg, att.Filename, att.Content, caption); err != nil {
			c.replyTo(ctx, msg, fmt.Sprintf("❌ 文件发送失败: %v\n\n%s",
				err, command.FormatReport(result)))
			return
		}
		c.maybeRegisterPending(result, text)
		return
	}

	report := command.FormatReport(result)
	c.replyTo(ctx, msg, report)
	c.maybeRegisterPending(result, text)
}

// ─────────────────────────────────────────────────────────────────────
// AI handlers
// ─────────────────────────────────────────────────────────────────────

func (c *Client) handleAIStart(ctx context.Context, msg *models.Message, userText string) {
	// Lazy-init: if Assistant wasn't set up at agent start (because AI was
	// not yet configured), try to build it now from current identity.json.
	// This makes /w ai key/provider take effect without restarting the agent.
	c.maybeInitAssistant()

	if c.Assistant == nil {
		c.replyTo(ctx, msg,
			"🤖 AI 未配置。请先用 /w ai key sk-xxx 设置 OpenAI API key，再用 /w ai provider openai 启用。")
		return
	}

	userID := userIDOf(msg)
	c.Assistant.Reset(userID) // fresh conversation per @mention

	// Strip the bot mention from the user's question
	question := strings.TrimSpace(userText)
	if question == "" {
		c.replyTo(ctx, msg, "🤖 嗨。问我什么？例如：\n  - 帮我把 example.com 加进 CDN，源站 1.2.3.4\n  - 当前节点状态\n  - 怎么申请证书")
		return
	}

	// Show "thinking..." indicator (could use sendChatAction for production)
	resp, err := c.Assistant.Start(ctx, userID, question)
	if err != nil {
		c.replyTo(ctx, msg, fmt.Sprintf("❌ AI 调用失败: %v", err))
		return
	}

	c.deliverAIResponse(ctx, msg, resp)
}

func (c *Client) handleAIContinue(ctx context.Context, msg *models.Message, userText string) {
	userID := userIDOf(msg)
	resp, err := c.Assistant.Continue(ctx, userID, userText)
	if err != nil {
		c.replyTo(ctx, msg, fmt.Sprintf("❌ AI 调用失败: %v", err))
		return
	}
	c.deliverAIResponse(ctx, msg, resp)
}

// deliverAIResponse processes an LLM response: extract command blocks,
// present them with execute/cancel buttons, and show the prose part.
func (c *Client) deliverAIResponse(ctx context.Context, msg *models.Message, resp string) {
	commands, prose := extractCommandBlocks(resp)

	// Always show the prose part (even if empty, send something)
	if prose != "" {
		c.replyTo(ctx, msg, prose)
	}

	if len(commands) == 0 {
		if prose == "" {
			c.replyTo(ctx, msg, "(AI 没有给出建议)")
		}
		return
	}

	// Present each command block with execute/cancel buttons
	for _, cmdText := range commands {
		execID := generateExecID()
		c.pendingExecMu.Lock()
		c.pendingExec[execID] = cmdText
		c.pendingExecMu.Unlock()

		// Schedule expiry sweep
		go func(id string) {
			time.Sleep(10 * time.Minute)
			c.pendingExecMu.Lock()
			delete(c.pendingExec, id)
			c.pendingExecMu.Unlock()
		}(execID)

		text := "🤖 AI 建议执行：\n\n```\n" + cmdText + "\n```\n\n点击下方按钮选择："
		c.replyWithButtons(ctx, msg, text, [][]ButtonData{
			{
				{Text: "✅ 执行", Command: "exec:" + execID},
				{Text: "❌ 取消", Command: "cancel:" + execID},
			},
		})
	}
}

// extractCommandBlocks parses LLM output for ```command ... ``` blocks.
// Returns (extracted commands, remaining prose).
func extractCommandBlocks(text string) (cmds []string, prose string) {
	// Match ```command\n...\n``` (and also ``` without language tag)
	re := regexp.MustCompile("(?s)```(?:command)?\\s*\n?(.*?)```")
	matches := re.FindAllStringSubmatchIndex(text, -1)

	var sb strings.Builder
	last := 0
	for _, m := range matches {
		// m[0]:m[1] = full match; m[2]:m[3] = first group
		sb.WriteString(text[last:m[0]])
		body := strings.TrimSpace(text[m[2]:m[3]])
		if body != "" {
			cmds = append(cmds, body)
		}
		last = m[1]
	}
	sb.WriteString(text[last:])
	prose = strings.TrimSpace(sb.String())
	return
}

// ─────────────────────────────────────────────────────────────────────
// Inline keyboard
// ─────────────────────────────────────────────────────────────────────

// ButtonData for replyWithButtons.
type ButtonData struct {
	Text    string
	Command string // sent back as callback_data
}

// replyWithButtons sends a text reply with inline keyboard buttons.
func (c *Client) replyWithButtons(ctx context.Context, original *models.Message,
	text string, rows [][]ButtonData) {

	c.mu.Lock()
	b := c.bot
	ready := c.ready
	c.mu.Unlock()
	if !ready || b == nil {
		return
	}

	keyboard := make([][]models.InlineKeyboardButton, len(rows))
	for i, row := range rows {
		keyboard[i] = make([]models.InlineKeyboardButton, len(row))
		for j, btn := range row {
			keyboard[i][j] = models.InlineKeyboardButton{
				Text:         btn.Text,
				CallbackData: btn.Command,
			}
		}
	}

	for _, chunk := range chunkText(text, 4000) {
		_, err := b.SendMessage(ctx, &tgbot.SendMessageParams{
			ChatID:          c.GroupID,
			ReplyParameters: &models.ReplyParameters{MessageID: original.ID},
			Text:            chunk,
			ReplyMarkup: &models.InlineKeyboardMarkup{
				InlineKeyboard: keyboard,
			},
		})
		if err != nil {
			log.Printf("[bot] reply with buttons: %v", err)
			return
		}
	}
}

// handleCallback processes inline button presses.
func (c *Client) handleCallback(ctx context.Context, b *tgbot.Bot, cq *models.CallbackQuery) {
	// Acknowledge the callback (removes the loading spinner)
	_, _ = b.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{
		CallbackQueryID: cq.ID,
	})

	data := cq.Data
	if data == "" {
		return
	}

	// Special: "exec:<id>" / "cancel:<id>" for AI command execution
	if strings.HasPrefix(data, "exec:") {
		execID := strings.TrimPrefix(data, "exec:")
		c.pendingExecMu.Lock()
		cmdText, ok := c.pendingExec[execID]
		delete(c.pendingExec, execID)
		c.pendingExecMu.Unlock()
		if !ok {
			c.SendToGroup(ctx, "⚠️ 该 AI 建议已过期或已处理")
			return
		}
		// Execute as if user typed it
		result, err := c.Executor.ExecuteBatch(ctx, cmdText)
		if err != nil {
			c.SendToGroup(ctx, fmt.Sprintf("❌ 执行失败: %v", err))
			return
		}
		c.SendToGroup(ctx, "🤖 AI 建议已执行：\n\n"+command.FormatReport(result))
		c.maybeRegisterPending(result, cmdText)
		return
	}
	if strings.HasPrefix(data, "cancel:") {
		execID := strings.TrimPrefix(data, "cancel:")
		c.pendingExecMu.Lock()
		delete(c.pendingExec, execID)
		c.pendingExecMu.Unlock()
		c.SendToGroup(ctx, "已取消")
		return
	}

	// V4.0.19: config-import buttons.
	// import-apply:<id>  → run the stashed batch
	// import-cancel:<id> → drop the stash
	if strings.HasPrefix(data, "import-apply:") {
		importID := strings.TrimPrefix(data, "import-apply:")
		c.pendingExecMu.Lock()
		batchText, ok := c.pendingExec["import:"+importID]
		delete(c.pendingExec, "import:"+importID)
		c.pendingExecMu.Unlock()
		if !ok {
			c.SendToGroup(ctx, "⚠️ 该导入请求已过期或已处理")
			return
		}
		c.SendToGroup(ctx, "📥 开始应用配置...")
		result, err := c.Executor.ExecuteBatch(ctx, batchText)
		if err != nil {
			c.SendToGroup(ctx, fmt.Sprintf("❌ 导入失败: %v", err))
			return
		}
		c.SendToGroup(ctx, command.FormatReport(result))
		c.maybeRegisterPending(result, batchText)
		return
	}
	if strings.HasPrefix(data, "import-cancel:") {
		importID := strings.TrimPrefix(data, "import-cancel:")
		c.pendingExecMu.Lock()
		delete(c.pendingExec, "import:"+importID)
		c.pendingExecMu.Unlock()
		c.SendToGroup(ctx, "已取消导入")
		return
	}

	// Regular menu button — treat as if user typed the command
	if cq.Message.Message == nil {
		return
	}
	msg := cq.Message.Message
	if strings.HasPrefix(data, "/menu") {
		rest := strings.TrimSpace(strings.TrimPrefix(data, "/menu"))
		c.showMenu(ctx, msg, rest)
		return
	}
	if strings.HasPrefix(data, "/") {
		c.handleTextCommand(ctx, b, msg, data)
	}
}

// ─────────────────────────────────────────────────────────────────────
// File upload (sslfile)
// ─────────────────────────────────────────────────────────────────────

func (c *Client) handleDocument(ctx context.Context, b *tgbot.Bot, msg *models.Message) {
	if c.FileFetcher == nil {
		c.replyTo(ctx, msg, "❌ file fetcher 未配置")
		return
	}

	caption := strings.TrimSpace(msg.Caption)

	// Route 1: explicit cert upload — caption starts with /w sslfile
	if strings.HasPrefix(caption, "/w sslfile ") {
		c.handleSSLFileUpload(ctx, b, msg)
		return
	}

	// V4.0.20: peek at the file content to decide routing for uncaptioned
	// uploads. PEM-shaped files always go to SSL upload (even without
	// caption); only non-PEM goes to config-import.
	//
	// This:
	//   1) Fixes v4.0.19's regression where a captionless .key file got
	//      routed to parseImportFile and confused the user.
	//   2) Lets users upload cert+key without typing /w sslfile each time —
	//      they can drag both files in, content sniff figures out the slot,
	//      and the 5-min buffer pairs them.
	//
	// We download the file once here and reuse the bytes downstream so we
	// don't pay the Telegram round-trip twice.
	doc := msg.Document
	if doc == nil {
		return
	}
	content, err := c.FileFetcher.Fetch(ctx, doc.FileID)
	if err != nil {
		c.replyTo(ctx, msg, fmt.Sprintf("❌ 下载失败: %v", err))
		return
	}

	if looksLikePEM(content) {
		c.handleSSLFileUploadWithBytes(ctx, msg, content)
		return
	}

	// Route 2: config import — empty caption or explicit /import
	if caption == "" || caption == "/import" || caption == "/w import" {
		c.handleConfigImportWithBytes(ctx, msg, content)
		return
	}

	c.replyTo(ctx, msg,
		"ℹ️ 上传文件支持两种用途:\n"+
			"  - 证书上传: 直接发 PEM 格式的 .crt/.key 文件 (无需 caption,系统自动识别)\n"+
			"  - 配置导入: 留空 caption (或写 `/import`),系统会显示预览+确认按钮")
}

// handleSSLFileUpload is the legacy entry point — kept for the explicit
// "/w sslfile <scope> -" caption path. Reads scope from caption, sniffs
// content for slot.
func (c *Client) handleSSLFileUpload(ctx context.Context, b *tgbot.Bot, msg *models.Message) {
	doc := msg.Document
	if doc == nil {
		return
	}
	content, err := c.FileFetcher.Fetch(ctx, doc.FileID)
	if err != nil {
		c.replyTo(ctx, msg, fmt.Sprintf("❌ 下载失败: %v", err))
		return
	}
	c.handleSSLFileUploadWithBytes(ctx, msg, content)
}

// handleSSLFileUploadWithBytes is the shared SSL-upload core, used by both
// caption-driven and content-sniff dispatch. Already-downloaded bytes are
// passed in so we don't re-fetch.
func (c *Client) handleSSLFileUploadWithBytes(ctx context.Context, msg *models.Message, content []byte) {
	doc := msg.Document
	filename := "(no name)"
	if doc != nil && doc.FileName != "" {
		filename = doc.FileName
	}

	// V4.0.20: sniff PEM content for slot. Filename / extension is informational
	// only — we never use it to decide cert vs key.
	sniff, err := sniffPEMSlot(content)
	if err != nil {
		c.replyTo(ctx, msg, fmt.Sprintf("❌ %s: %v", filename, err))
		return
	}

	// Caption may carry "/w sslfile <scope> -". If absent, we'll still buffer
	// the file — when its pair arrives, we'll need a scope, so we synthesize
	// one from the cert's CN at execute time. The caption-driven path stays
	// fully backward-compatible.
	caption := strings.TrimSpace(msg.Caption)

	userID := userIDOf(msg)
	c.uploadBuffers.put(userID, sniff.Slot, string(content), caption)
	certPEM, keyPEM, savedCaption := c.uploadBuffers.get(userID)

	if certPEM == "" || keyPEM == "" {
		need := "私钥 PEM"
		if certPEM == "" {
			need = "证书 PEM"
		}
		c.replyTo(ctx, msg, fmt.Sprintf("📁 已收到 %s (识别为 %s),等待 %s (5 分钟)",
			filename, sniff.Slot, need))
		return
	}

	// Both halves present. Resolve the command:
	//   - If buffer carries a "/w sslfile <scope> -" caption, use it as-is.
	//   - Otherwise, synthesize from the cert's CN.
	cmdText := savedCaption
	if !strings.HasPrefix(cmdText, "/w sslfile ") {
		scope := extractCertCN([]byte(certPEM))
		if scope == "" {
			c.uploadBuffers.clear(userID)
			c.replyTo(ctx, msg,
				"❌ 证书没有 Common Name,无法自动确定 scope。请重新上传并加 caption: /w sslfile <域名> -")
			return
		}
		cmdText = fmt.Sprintf("/w sslfile %s -", scope)
	}

	c.uploadBuffers.clear(userID)
	if err := executeWithUploadEnv(ctx, c.Executor, cmdText, certPEM, keyPEM); err != nil {
		c.replyTo(ctx, msg, fmt.Sprintf("❌ 上传执行失败: %v", err))
		return
	}
	c.replyTo(ctx, msg, fmt.Sprintf("✅ 证书已上传 (`%s`)", cmdText))
}

// handleConfigImportWithBytes is the shared config-import core, used when
// content sniff has already happened. Bytes are passed in to avoid re-fetch.
func (c *Client) handleConfigImportWithBytes(ctx context.Context, msg *models.Message, content []byte) {
	doc := msg.Document
	filename := "(无文件名)"
	if doc != nil && doc.FileName != "" {
		filename = doc.FileName
	}

	const maxImportSize = 2 * 1024 * 1024
	if int64(len(content)) > maxImportSize {
		c.replyTo(ctx, msg,
			fmt.Sprintf("❌ 文件太大 (%d KB > 2 MB 上限)。配置文件几乎不应该超过 2 MB,请确认文件类型",
				len(content)/1024))
		return
	}

	parsed := parseImportFile(content)
	total := parsed.WriteCount + parsed.DeleteCount

	if total == 0 {
		c.replyTo(ctx, msg, parsed.summary(filename)+
			"\n\n如果这是别的用途的文件,请加上对应 caption 重新上传。")
		return
	}

	importID := generateConfirmID()
	c.pendingExecMu.Lock()
	if c.pendingExec == nil {
		c.pendingExec = make(map[string]string)
	}
	c.pendingExec["import:"+importID] = parsed.batchText()
	c.pendingExecMu.Unlock()

	c.replyWithButtons(ctx, msg, parsed.summary(filename), [][]ButtonData{
		{
			{Text: "✅ 应用", Command: "import-apply:" + importID},
			{Text: "❌ 取消", Command: "import-cancel:" + importID},
		},
	})
}

// handleConfigImport is the legacy entry point for explicit caption-driven
// config imports. Downloads then forwards to the shared core.
func (c *Client) handleConfigImport(ctx context.Context, b *tgbot.Bot, msg *models.Message) {
	doc := msg.Document
	if doc == nil {
		return
	}
	const maxImportSize = 2 * 1024 * 1024
	if doc.FileSize > maxImportSize {
		c.replyTo(ctx, msg,
			fmt.Sprintf("❌ 文件太大 (%d KB > 2 MB 上限)。配置文件几乎不应该超过 2 MB,请确认文件类型",
				doc.FileSize/1024))
		return
	}
	content, err := c.FileFetcher.Fetch(ctx, doc.FileID)
	if err != nil {
		c.replyTo(ctx, msg, fmt.Sprintf("❌ 下载失败: %v", err))
		return
	}
	c.handleConfigImportWithBytes(ctx, msg, content)
}

// ─────────────────────────────────────────────────────────────────────
// Pending confirmations (port conflict / cascade delete)
// ─────────────────────────────────────────────────────────────────────

func (c *Client) maybeRegisterPending(result *command.BatchResult, originalText string) {
	if c.PendingStore == nil {
		return
	}
	for _, o := range result.Outcomes {
		if o.Err == nil {
			continue
		}
		ce, ok := o.Err.(command.CommandError)
		if !ok {
			continue
		}
		switch ce.Code() {
		case command.ErrCascadeBlocked, command.ErrPortConflict:
			id := generateConfirmID()
			c.PendingStore.Add(id, originalText)
			c.SendToGroup(context.Background(),
				fmt.Sprintf("⚠️  操作被阻止。强制执行回复:\n  /v confirm %s -", id))
		}
	}
}

// ─────────────────────────────────────────────────────────────────────
// Reply helpers
// ─────────────────────────────────────────────────────────────────────

func (c *Client) replyTo(ctx context.Context, original *models.Message, text string) {
	c.mu.Lock()
	b := c.bot
	ready := c.ready
	c.mu.Unlock()
	if !ready || b == nil {
		return
	}
	if text == "" {
		text = "(no output)"
	}
	for _, chunk := range chunkText(text, 4000) {
		_, err := b.SendMessage(ctx, &tgbot.SendMessageParams{
			ChatID:          c.GroupID,
			ReplyParameters: &models.ReplyParameters{MessageID: original.ID},
			Text:            chunk,
		})
		if err != nil {
			log.Printf("[bot] reply: %v", err)
			return
		}
	}
}

// replyWithFile sends a file as a Telegram document, replying to `original`,
// with the given filename, content bytes, and caption.
//
// Caption is capped at Telegram's 1024-char limit; longer messages are sent
// as a separate follow-up text message after the document.
func (c *Client) replyWithFile(ctx context.Context, original *models.Message,
	filename string, content []byte, caption string) error {

	c.mu.Lock()
	b := c.bot
	ready := c.ready
	c.mu.Unlock()
	if !ready || b == nil {
		return errors.New("bot not ready")
	}

	const captionLimit = 1024
	primaryCaption := caption
	overflow := ""
	if len(primaryCaption) > captionLimit {
		// Cut at a newline boundary if possible
		cut := captionLimit
		if idx := strings.LastIndex(primaryCaption[:captionLimit], "\n"); idx > captionLimit/2 {
			cut = idx
		}
		overflow = primaryCaption[cut:]
		primaryCaption = primaryCaption[:cut]
	}

	_, err := b.SendDocument(ctx, &tgbot.SendDocumentParams{
		ChatID:          c.GroupID,
		ReplyParameters: &models.ReplyParameters{MessageID: original.ID},
		Document: &models.InputFileUpload{
			Filename: filename,
			Data:     bytes.NewReader(content),
		},
		Caption: primaryCaption,
	})
	if err != nil {
		return fmt.Errorf("sendDocument: %w", err)
	}

	if overflow != "" {
		c.replyTo(ctx, original, strings.TrimSpace(overflow))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────
// Misc helpers
// ─────────────────────────────────────────────────────────────────────

func chunkText(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= limit {
			chunks = append(chunks, text)
			break
		}
		split := limit
		if nl := strings.LastIndex(text[:limit], "\n"); nl > limit/2 {
			split = nl + 1
		}
		chunks = append(chunks, text[:split])
		text = text[split:]
	}
	return chunks
}

func stripBotMention(s string) string {
	// Strip both "@botname" forms and Telegram's slash-suffix "/cmd@botname"
	if !strings.HasPrefix(s, "/") && !strings.Contains(s, "@") {
		return s
	}
	at := strings.Index(s, "@")
	if at < 0 {
		return s
	}
	sp := strings.IndexAny(s[at:], " \n")
	if sp < 0 {
		return s[:at]
	}
	return s[:at] + s[at+sp:]
}

func generateConfirmID() string {
	return fmt.Sprintf("c%d", time.Now().UnixNano()%1000000)
}

func generateExecID() string {
	return fmt.Sprintf("e%d", time.Now().UnixNano()%1000000)
}

func userIDOf(msg *models.Message) int64 {
	if msg.From == nil {
		return 0
	}
	return msg.From.ID
}

// maybeInitAssistant lazily initializes c.Assistant from the current
// identity.json when AI was not configured at agent startup.
//
// This means users can run `/w ai key ...` and `/w ai provider ...` while
// the agent is running, and the next @mention will pick up the new config
// without needing a service restart.
//
// Once Assistant is non-nil, this is a no-op. To "re-load" after changing
// AI config (e.g. switching providers or rotating keys), restart the agent.
func (c *Client) maybeInitAssistant() {
	c.mu.Lock()
	if c.Assistant != nil {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	id, err := identity.Load()
	if err != nil {
		return
	}
	if !id.AIConfigured() {
		return
	}
	provider, err := ai.NewProvider(id.AIProvider, id.GetAPIKey(id.AIProvider), id.AIActiveModel())
	if err != nil {
		log.Printf("[ai] lazy init failed: %v", err)
		return
	}

	c.mu.Lock()
	if c.Assistant == nil {
		c.Assistant = ai.NewAssistant(provider, ai.SystemPrompt)
		log.Printf("[ai] enabled via lazy init (provider=%s model=%s)",
			id.AIProvider, id.AIActiveModel())
	}
	c.mu.Unlock()
}
