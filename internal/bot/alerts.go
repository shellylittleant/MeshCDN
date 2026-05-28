// Bot-backed alert sink.
//
// Per V4-DESIGN §1.6: the event stream carries non-version-tracked messages
// like cert renewal failures. The mesh server hands incoming events to a
// configured handler; serve.go wires the bot's TelegramAlertSink to that
// handler.
//
// The renew worker (step 3) takes a renew.AlertSink interface; bot also
// satisfies that — so cert renewal alerts can flow:
//
//	renew.Worker.AlertSink → telegramAlertSink.Alert(...)
//	                       ↓
//	                   Bot.SendToGroup("🚨 cert renewal failed: ...")
package bot

import (
	"context"
	"fmt"
	"time"
)

// TelegramAlertSink implements renew.AlertSink by formatting and sending
// to the bot's group.
//
// It's safe to construct one even if Bot is nil (writes to log instead).
// This means the renew worker can always have an AlertSink wired without
// caring whether the bot is up.
type TelegramAlertSink struct {
	Bot *Client
}

// Alert is called by the renew worker on cert renewal failure.
//
// Format follows V4-DESIGN §3.7 step 3 minimalism:
//   - identifier + source + expiry
//   - original error string (no "建议" line — caller decides what to do)
//   - 24h dedup is handled inside renew.Worker, not here
func (s *TelegramAlertSink) Alert(identifier, source string, notAfter time.Time, errMsg string) {
	if s == nil || s.Bot == nil {
		// Fall back to log output — the standard renew.LoggerSink already
		// does this, so we skip here to avoid double-logging.
		return
	}
	expiry := "(unknown)"
	if !notAfter.IsZero() {
		expiry = notAfter.Format("2006-01-02 15:04 MST")
		remaining := time.Until(notAfter)
		days := int(remaining.Hours() / 24)
		expiry = fmt.Sprintf("%s (剩余 %d 天)", expiry, days)
	}

	text := fmt.Sprintf(
		"🚨 证书续签失败\n\n"+
			"域名: %s\n"+
			"来源: %s\n"+
			"到期: %s\n\n"+
			"错误: %s",
		identifier, source, expiry, errMsg)

	s.Bot.SendToGroup(context.Background(), text)
}

// HandleMeshEvent is the function we wire to mesh.Server.OnEvent.
// Forwards qualifying events into Telegram messages.
//
// Step 7 forwards types: alert, peer-joined, peer-lost, bot-transferred.
// The challenge-share type is handled separately by ACME and not user-visible.
func (s *TelegramAlertSink) HandleMeshEvent(eventType, fromIP string, payload map[string]interface{}) {
	if s == nil || s.Bot == nil {
		return
	}
	switch eventType {
	case "alert":
		// Already-formatted text in payload["text"]
		if text, ok := payload["text"].(string); ok {
			s.Bot.SendToGroup(context.Background(), "📢 "+text+"\n来自: "+fromIP)
		}
	case "peer-joined":
		s.Bot.SendToGroup(context.Background(),
			fmt.Sprintf("➕ 新节点加入集群: %s", fromIP))
	case "peer-lost":
		s.Bot.SendToGroup(context.Background(),
			fmt.Sprintf("➖ 节点失联: %s", fromIP))
	case "bot-transferred":
		newBot := ""
		if v, ok := payload["new_bot"].(string); ok {
			newBot = v
		}
		s.Bot.SendToGroup(context.Background(),
			fmt.Sprintf("🔄 Bot 角色已转移: %s → %s", fromIP, newBot))
	default:
		// Unknown event types are silently ignored
	}
}
