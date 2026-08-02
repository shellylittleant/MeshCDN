// Access-log collector — the "4th loop" alongside heartbeat / renewal / events
// (V4-DESIGN §A.3). Every interval it reads the new tail of nginx's JSON stats
// log, aggregates it by (domain, minute, status), and commits to logs.db.
//
// Design (V4-DESIGN task 3, option A): periodic incremental tail, not a
// log_by_lua hot-path hook — parsing stays off nginx's request path and a
// minute of aggregation delay is acceptable for traffic stats. The consumed
// byte offset is persisted in logs.db, so an agent restart never re-counts
// already-aggregated lines.
package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"time"
)

// Default cadence and retention.
const (
	DefaultInterval  = time.Minute
	DefaultRetention = 7 * 24 * time.Hour
)

// Collector reads LogPath incrementally into Store.
type Collector struct {
	Store     *Store
	LogPath   string
	Interval  time.Duration
	Retention time.Duration
}

// logLine mirrors the nginx `escape=json` log_format (see nginx/templates.go).
type logLine struct {
	Time   string `json:"t"`
	Host   string `json:"host"`
	Status int    `json:"status"`
	Bytes  int64  `json:"bytes"`
}

// Run ticks until ctx is canceled. First collection happens immediately so a
// freshly-started agent flushes any backlog written before it came up.
func (c *Collector) Run(ctx context.Context) {
	interval := c.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	if err := c.CollectOnce(ctx); err != nil {
		log.Printf("[logs/collector] initial collect: %v", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.CollectOnce(ctx); err != nil {
				log.Printf("[logs/collector] collect: %v", err)
			}
		}
	}
}

// CollectOnce reads all complete new lines from LogPath, aggregates them, and
// commits atomically with the advanced offset. A partial trailing line (no
// newline yet) is left for the next tick. Also prunes expired aggregates.
func (c *Collector) CollectOnce(ctx context.Context) error {
	f, err := os.Open(c.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nginx hasn't created the log yet
		}
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()

	offset, err := c.Store.Offset(ctx)
	if err != nil {
		return err
	}
	// Log rotated or truncated → start over from the top.
	if offset > size {
		offset = 0
		if err := c.Store.ResetOffset(ctx, 0); err != nil {
			return err
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}

	deltas := make(map[StatKey]StatDelta)
	consumed := offset
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			// EOF with no trailing newline → partial line, leave it unconsumed.
			break
		}
		consumed += int64(len(line))
		c.accumulate(deltas, line)
	}

	if len(deltas) == 0 && consumed == offset {
		return c.prune(ctx)
	}
	if err := c.Store.Commit(ctx, deltas, consumed); err != nil {
		return err
	}
	return c.prune(ctx)
}

func (c *Collector) accumulate(deltas map[StatKey]StatDelta, raw []byte) {
	var ll logLine
	if err := json.Unmarshal(raw, &ll); err != nil {
		return // skip malformed lines rather than abort the whole batch
	}
	if ll.Host == "" || ll.Status == 0 {
		return
	}
	minute := parseMinute(ll.Time)
	if minute == 0 {
		return
	}
	k := StatKey{Domain: ll.Host, Minute: minute, Status: ll.Status}
	d := deltas[k]
	d.Hits++
	if ll.Bytes > 0 {
		d.Bytes += ll.Bytes
	}
	deltas[k] = d
}

func (c *Collector) prune(ctx context.Context) error {
	retention := c.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	cutoff := time.Now().Add(-retention).Truncate(time.Minute).Unix()
	if _, err := c.Store.Prune(ctx, cutoff); err != nil {
		return err
	}
	return nil
}

// parseMinute parses nginx $time_iso8601 ("2026-07-01T13:04:05+08:00") and
// truncates to the minute (unix epoch). Returns 0 on parse failure.
func parseMinute(s string) int64 {
	t, err := time.Parse("2006-01-02T15:04:05Z07:00", s)
	if err != nil {
		return 0
	}
	return t.Truncate(time.Minute).Unix()
}
