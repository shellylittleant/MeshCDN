// Bot config import — parses uploaded .txt files of commands and routes them
// through Executor for application.
//
// V4.0.19 addition. The symmetry is intentional: /v export produces a .txt
// of commands; uploading that .txt (no caption / caption = /import) applies
// it back. The whole pipeline reuses the existing executor so mesh broadcast,
// idempotency, and audit semantics are identical to typed commands.
//
// Trust model: any group member can upload. Same trust boundary as typing
// commands. We do NOT add a separate ACL check; if you don't trust someone
// to type /w domain, don't add them to the group.
package bot

import (
	"fmt"
	"strings"
)

// parsedImport summarizes a config file upload before user confirmation.
type parsedImport struct {
	// CommandLines are the lines we'll feed to ExecuteBatch (already
	// stripped of comments / view commands / blank lines).
	CommandLines []string

	// Counts surface in the confirmation prompt.
	WriteCount   int // /w lines
	DeleteCount  int // /d lines
	SkippedView  int // /v lines we silently dropped
	CommentCount int // # comment lines and blanks (purely informational)

	// MalformedLines are lines that didn't match /w, /d, /v, or comment.
	// Reported in the confirmation but NOT executed.
	MalformedLines []malformedLine
}

type malformedLine struct {
	LineNo  int    // 1-indexed
	Content string // truncated for display
}

// parseImportFile takes the raw uploaded file content and returns a parsed
// summary. The parser is permissive on whitespace and case but strict on
// command verb prefix.
func parseImportFile(body []byte) *parsedImport {
	out := &parsedImport{}

	// Strip UTF-8 BOM if present
	text := string(body)
	if strings.HasPrefix(text, "\ufeff") {
		text = strings.TrimPrefix(text, "\ufeff")
	}
	// Normalize line endings
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	for i, rawLine := range strings.Split(text, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(rawLine)

		if line == "" {
			out.CommentCount++
			continue
		}
		if strings.HasPrefix(line, "#") {
			out.CommentCount++
			continue
		}

		switch {
		case strings.HasPrefix(line, "/w "):
			out.WriteCount++
			out.CommandLines = append(out.CommandLines, line)
		case strings.HasPrefix(line, "/d "):
			out.DeleteCount++
			out.CommandLines = append(out.CommandLines, line)
		case strings.HasPrefix(line, "/v "):
			// View commands don't belong in a config file. Drop silently
			// but count for the summary.
			out.SkippedView++
		default:
			disp := line
			if len(disp) > 80 {
				disp = disp[:77] + "..."
			}
			out.MalformedLines = append(out.MalformedLines, malformedLine{
				LineNo:  lineNo,
				Content: disp,
			})
		}
	}
	return out
}

// summary renders the user-facing preview shown before the confirm button.
func (p *parsedImport) summary(filename string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📥 收到配置文件 %s\n\n", filename)
	fmt.Fprintf(&sb, "  ✏️  /w 写入命令:  %d 条\n", p.WriteCount)
	if p.DeleteCount > 0 {
		fmt.Fprintf(&sb, "  🗑  /d 删除命令:  %d 条\n", p.DeleteCount)
	}
	if p.SkippedView > 0 {
		fmt.Fprintf(&sb, "  👁  /v 跳过:      %d 条 (视图命令不属于配置)\n", p.SkippedView)
	}
	if len(p.MalformedLines) > 0 {
		fmt.Fprintf(&sb, "  ⚠️  无法识别:    %d 行 (将跳过)\n", len(p.MalformedLines))
		// Show up to first 5 malformed lines
		shown := p.MalformedLines
		if len(shown) > 5 {
			shown = shown[:5]
		}
		for _, m := range shown {
			fmt.Fprintf(&sb, "      L%d: %s\n", m.LineNo, m.Content)
		}
		if len(p.MalformedLines) > 5 {
			fmt.Fprintf(&sb, "      ... 还有 %d 行未显示\n", len(p.MalformedLines)-5)
		}
	}
	total := p.WriteCount + p.DeleteCount
	if total == 0 {
		sb.WriteString("\n  ❌ 文件中没有可执行的命令")
	} else {
		fmt.Fprintf(&sb, "\n  将顺序执行 %d 条命令。中途失败的命令会被记录但不会回滚已成功的。",
			total)
	}
	return sb.String()
}

// batchText joins CommandLines into the multi-line format ExecuteBatch expects.
func (p *parsedImport) batchText() string {
	return strings.Join(p.CommandLines, "\n")
}
