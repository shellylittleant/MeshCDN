// Mesh-based alert sink — used by NON-bot nodes.
//
// V4 architecture: only the bot node connects to api.telegram.org. Worker
// nodes (those that ran with MESHCDN_BOT_DISABLE=1, or those whose bot
// startup failed because they can't reach Telegram) cannot send alerts
// directly. Instead, they forward each alert through the mesh event
// stream to the bot node, which then posts to the Telegram group.
//
// This is the implementation of "internal forwarding": worker nodes don't
// need any direct Telegram access; they only talk to peers over mesh:9443.
//
// Per V4-DESIGN §1.6: events are best-effort. If the bot node is briefly
// unreachable, we drop the event and log locally. The renew worker's own
// 24-hour dedup means the alert will be re-attempted next scan if the
// underlying problem persists.
package bot

import (
	"context"
	"log"
	"time"

	"github.com/example/meshcdn/internal/mesh"
)

// MeshAlertSink forwards alerts to the bot node as mesh /mesh/event messages.
//
// Wiring: serve.go picks this when our peer is NOT the bot node.
type MeshAlertSink struct {
	// Client is the mesh client used to POST events.
	Client *mesh.Client

	// Port is the mesh port on the bot node (typically 9443).
	Port int

	// LocalNodeIP is our own IP, included in the event payload so the bot
	// node can mention "alert from <peer>" in the Telegram message.
	LocalNodeIP string

	// BotNodeIP is the resolver: returns the current bot node's IP.
	// Wired by serve.go using peers.json (peer with smallest join_order).
	// Returns empty string if no bot node is currently elected — in that
	// case the alert is dropped (logged only).
	BotNodeIP func() string
}

// Alert implements renew.AlertSink.
func (s *MeshAlertSink) Alert(identifier, source string, notAfter time.Time, errMsg string) {
	if s == nil || s.Client == nil || s.BotNodeIP == nil {
		log.Printf("[mesh-alert] sink not wired; dropping alert: %s", errMsg)
		return
	}

	botIP := s.BotNodeIP()
	if botIP == "" {
		log.Printf("[mesh-alert] no bot node known; dropping alert (id=%s err=%s)",
			identifier, errMsg)
		return
	}
	// If we ARE the bot node, this sink shouldn't be wired. But guard anyway.
	if botIP == s.LocalNodeIP {
		log.Printf("[mesh-alert] WARN: this node is bot node but using MeshAlertSink; alert lost")
		return
	}

	port := s.Port
	if port == 0 {
		port = mesh.DefaultPort
	}

	// Format the alert as a single text payload. Bot node's HandleMeshEvent
	// type="alert" handler shows payload["text"].
	expiryStr := "(unknown)"
	if !notAfter.IsZero() {
		days := int(time.Until(notAfter).Hours() / 24)
		expiryStr = notAfter.Format("2006-01-02") + " (剩余 " + itoa(days) + " 天)"
	}
	text := "🚨 证书续签失败\n" +
		"域名: " + identifier + "\n" +
		"来源: " + source + "\n" +
		"到期: " + expiryStr + "\n" +
		"错误: " + errMsg

	msg := &mesh.EventMessage{
		FromIP: s.LocalNodeIP,
		Type:   "alert",
		Payload: map[string]interface{}{
			"text": text,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.Client.Event(ctx, botIP, port, msg); err != nil {
		log.Printf("[mesh-alert] failed to forward to bot node %s: %v", botIP, err)
		return
	}
	log.Printf("[mesh-alert] forwarded to bot node %s (id=%s)", botIP, identifier)
}

// itoa is a local int → string helper to avoid pulling strconv into this file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
