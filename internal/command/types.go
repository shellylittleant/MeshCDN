// Package command implements MeshCDN's command system — the "muscle layer"
// per V4 design philosophy.
//
// All external input (Telegram messages, CLI invocations, mesh broadcasts)
// converges here. This package defines:
//
//   - Command: the strict 4-segment form (verb + type + scope + params)
//   - Handler: the interface every resource type implements
//   - Effects: side-effects a command produces (nginx reload, cert work, events)
//   - Rule: the in-database representation of a single rule
//   - PendingConfirmation: state for the "warning + /confirm" UX
//
// New features add new Handlers; the verb dispatch, batch transaction, sync,
// and error-handling machinery never need to change. This is the design
// promise of the muscle layer.
//
// Reference: V4-DESIGN.md §0 (philosophy), §2 (transaction semantics),
// §8 (command structure).
package command

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/example/meshcdn/internal/i18n"
)

// ─────────────────────────────────────────────────────────────────────
// Verb
// ─────────────────────────────────────────────────────────────────────

// Verb is one of the three command verbs.
//
// w/d are mirror-symmetric: d <type> <scope> <params> precisely undoes
// w <type> <scope> <params> with the same primary key.
//
// v is the read-only counterpart; it follows the same 4-segment shape
// but never mutates state.
type Verb string

const (
	VerbWrite  Verb = "w"
	VerbDelete Verb = "d"
	VerbView   Verb = "v"
)

func (v Verb) IsMutating() bool { return v == VerbWrite || v == VerbDelete }

// ─────────────────────────────────────────────────────────────────────
// Command (parsed form, before handler-specific interpretation)
// ─────────────────────────────────────────────────────────────────────

// Command is the parsed result of a single command line.
//
// All commands are STRICTLY 4 segments. Empty positions use the literal
// dash "-" as a placeholder. The parser does not relax this rule.
type Command struct {
	Verb   Verb   // w / d / v
	Type   string // domain / ssl / cache / defense / bind / ...
	Scope  string // host:port / domain / IP / object-name / "-"
	Params string // type-specific params text (handler interprets) / "-"

	// Raw is the original command text, preserved for error messages
	// and for export/snapshot regeneration.
	Raw string

	// Lang is the interface language this command's output should be rendered
	// in. Populated by the executor from the request context; handlers read it
	// via cmd.T(). The Handler interface takes no context, and adding one to
	// every handler for a display concern would be the wrong trade — the
	// language belongs to the request, and the Command *is* the request.
	Lang i18n.Lang
}

// T renders a catalogue key in this command's language.
func (c *Command) T(key string, args ...interface{}) string {
	lang := c.Lang
	if !lang.Valid() {
		lang = i18n.Default
	}
	return i18n.T(lang, key, args...)
}

// Parse turns a single line of command text into a Command.
//
// Strict 4-segment form: split on whitespace into exactly 4 fields.
// First token must start with "/". The leading slash is stripped.
//
// Examples:
//
//	/w domain https://a.com:443 https://1.2.3.4:443
//	/d cache  img-7d            -
//	/v ssl    a.com             -
//	/v export -                 -
//	/w bind   a.com             cache:img-7d
func Parse(line string) (*Command, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, errors.New("empty command")
	}
	if !strings.HasPrefix(line, "/") {
		return nil, errors.New("commands must start with '/'")
	}
	line = strings.TrimPrefix(line, "/")

	// Split into exactly 4 fields. The 4th field captures the rest verbatim
	// so that handler-specific params (which may contain spaces) survive.
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return nil, fmt.Errorf("command must have 4 segments (verb type scope params), got %d", len(fields))
	}

	verbStr := fields[0]
	verb := Verb(verbStr)
	if verb != VerbWrite && verb != VerbDelete && verb != VerbView {
		return nil, fmt.Errorf("verb must be w/d/v, got %q", verbStr)
	}

	typeStr := fields[1]
	scope := fields[2]
	// params: everything after the third field (handlers may contain key=value sequences)
	params := strings.Join(fields[3:], " ")

	return &Command{
		Verb:   verb,
		Type:   typeStr,
		Scope:  scope,
		Params: params,
		Raw:    "/" + line,
	}, nil
}

// Placeholder is the sentinel for "any/none/default" per V4-DESIGN §8.1.
const Placeholder = "-"

// IsPlaceholder reports whether s is the literal "-" token.
func IsPlaceholder(s string) bool { return s == Placeholder }

// ─────────────────────────────────────────────────────────────────────
// Rule (in-DB shape; handler-specific structured form lives in Params)
// ─────────────────────────────────────────────────────────────────────

// Rule is the canonical in-database representation of one configured rule.
//
// Type + Scope + PrimaryKeyExtra together form the unique identity (the
// "primary key" per V4-DESIGN §8 mirror-symmetry rules). Two writes with
// the same primary key are an UPDATE; with different primary keys are
// independent rules.
//
// ParamsText is the original params text from the command, stored verbatim
// so /v export can round-trip it. Each handler also parses ParamsText into
// a typed structure for execution; that typed form is not stored in the DB.
type Rule struct {
	ID         int64
	Type       string
	Scope      string
	PrimaryKey string // computed: scope + handler-defined selectors

	ParamsText string

	// Bookkeeping
	ConfigVersion int64 // version at which this rule was last written
	UpdatedAt     time.Time
}

// ─────────────────────────────────────────────────────────────────────
// Effects (what a command produces beyond DB writes)
// ─────────────────────────────────────────────────────────────────────

// Effects is the bundle of side-effects a Handler.Execute reports back to
// the executor. The executor is responsible for actually carrying them out
// after the DB transaction commits.
//
// This separation matters: handlers never trigger nginx reloads or network
// I/O directly. They only declare what should happen. The executor batches
// effects across all commands in a batch and applies them once at the end.
// This guarantees that a failed mid-batch command cannot leave nginx in a
// partial state.
type Effects struct {
	// NeedsNginxReload triggers a single nginx reload after batch commit.
	// Multiple commands in a batch all setting this still result in ONE reload.
	NeedsNginxReload bool

	// NeedsCertReselect lists endpoints whose current selected cert may need
	// re-evaluation (per V4-DESIGN §3.6). E.g. a new /w sslfile or /w ssl
	// adds candidates and the algorithm should be re-run.
	NeedsCertReselect []string

	// EventNotifications enter the event stream (V4-DESIGN §1.6) — alerts,
	// challenge-share, bot-transfer signals, etc.
	EventNotifications []Event

	// PortProtocolBindings declares port→protocol bindings introduced by
	// this command. The batch-level pre-check verifies these don't conflict
	// with the existing global port_protocols table; conflicts trigger the
	// /confirm flow.
	PortProtocolBindings []PortProtocolBinding

	// ForceVersionBump makes the executor treat the batch as a config mutation
	// (bump config_version, export snapshot, notify peers) even when no /w or /d
	// ran. Used by read-shaped actions that nonetheless change synced cluster
	// state — e.g. /v target writing cluster_meta.bot_node_ip.
	ForceVersionBump bool

	// PurgeCache asks the executor to empty this node's proxy_cache directory
	// and reload nginx. It is an action effect (like /v sync): it carries no DB
	// mutation, so it does NOT bump config_version or enter the snapshot stream.
	PurgeCache bool

	// PurgeCacheBroadcast, when set alongside PurgeCache, tells the executor to
	// fan the purge out to every peer (each clears its own cache). Peers receive
	// the non-broadcasting form, so there is no re-broadcast storm.
	PurgeCacheBroadcast bool

	// UserMessage is the friendly response shown to the user (Telegram or CLI).
	// Should be 1-2 sentences for normal cases; may include emoji.
	UserMessage string

	// FileAttachment, if non-nil, asks the frontend to deliver the response
	// as a file rather than chat text. UserMessage is still shown as
	// accompanying caption / message. Used by /v export (v4.0.19+) to avoid
	// Telegram's 4096-char message limit and produce a re-importable artifact.
	//
	// Merge keeps the latest non-nil FileAttachment (last write wins). In
	// practice only one command in a batch produces a file, so collisions
	// don't occur.
	FileAttachment *FileAttachment
}

// FileAttachment describes a file the frontend should deliver alongside the
// command response. Bot frontends call sendDocument; CLI writes to stdout
// or a path.
type FileAttachment struct {
	Filename string // e.g. "meshcdn-export-v61-20260518T140000Z.txt"
	Content  []byte // raw bytes
	MIMEHint string // optional; e.g. "text/plain; charset=utf-8"
}

// PortProtocolBinding represents a port→protocol assertion derived from
// a /w domain command. Per V4-DESIGN §8 (port-protocol global rule),
// each port has exactly one protocol cluster-wide; conflicts require
// explicit /confirm.
type PortProtocolBinding struct {
	Port     int
	Protocol string // "http" or "https"
}

// Event is a single message on the event stream (non-config-version-tracked).
type Event struct {
	Type    string // "alert", "challenge-share", "bot-transferred", ...
	Payload map[string]interface{}
}

// ─────────────────────────────────────────────────────────────────────
// Handler interface (the muscle layer's DNA)
// ─────────────────────────────────────────────────────────────────────

// Handler is the contract every resource type implements. New resource
// types ("rate-limit", "auth", whatever the future brings) become first-class
// citizens by implementing this interface and registering in the Registry.
//
// The interface is deliberately minimal: only the steps that genuinely
// differ across types live here. Common machinery (parsing top-level command
// shape, batch transaction, event dispatch, snapshot export) is implemented
// once in this package and never duplicated per type.
type Handler interface {
	// Type returns the resource type identifier, e.g. "domain", "cache".
	// Must match Command.Type exactly.
	Type() string

	// PrimaryKey computes the unique identity for a rule given its scope
	// and parsed params. Two rules with the same Type and PrimaryKey are
	// treated as the same rule (write = update; both deletable by either).
	//
	// For most types: PrimaryKey = scope.
	// For cache: PrimaryKey = scope + pattern (so /w cache img-7d patterns=*.jpg
	//   and /w cache img-7d patterns=*.png are distinct rules sharing scope).
	//   Wait — under the object/binding model, cache objects key by name only.
	//   See handlers/cache.go for the actual computation.
	PrimaryKey(scope string, paramsText string) (string, error)

	// Validate performs pure, stateless checks on a parsed command:
	//   - scope format (is it a valid hostname / IP / object name?)
	//   - params syntax (does the key=value parse?)
	//   - field value ranges (TTL >= 0, etc.)
	// Validate must NOT touch the database or filesystem.
	Validate(cmd *Command) error

	// Write executes a /w command within a transaction. Returns Effects
	// describing what should happen after commit.
	//
	// Write is idempotent w.r.t. PrimaryKey: writing the same primary key
	// again replaces the previous rule (UPDATE semantics).
	Write(tx *sql.Tx, cmd *Command) (Effects, error)

	// Delete executes a /d command within a transaction.
	//
	// The handler matches by primary key; ParamsText in the command is
	// allowed to mismatch the stored rule (mirror-symmetric d commands
	// from /v export will match exactly; hand-typed d commands may be
	// loose). The handler decides how strict to be.
	//
	// If the rule does not exist, Delete returns a non-error empty Effects
	// with a UserMessage like "no such rule" — deleting non-existent
	// things is not an error condition.
	Delete(tx *sql.Tx, cmd *Command) (Effects, error)

	// View executes a /v command (read-only). Returns formatted output
	// for the user. tx is read-only; handlers must not mutate.
	View(tx *sql.Tx, cmd *Command) (Effects, error)
}

// Registry maps Type string → Handler. Populated at init time by
// internal/command/handlers.
type Registry map[string]Handler

// Get looks up a handler by type. Returns nil and an error if unknown.
func (r Registry) Get(t string) (Handler, error) {
	h, ok := r[t]
	if !ok {
		return nil, fmt.Errorf("unknown command type %q", t)
	}
	return h, nil
}

// ─────────────────────────────────────────────────────────────────────
// Batch execution (V4-DESIGN §2 transaction semantics)
// ─────────────────────────────────────────────────────────────────────

// BatchResult is what the executor returns for a multi-command input.
//
// Per V4-DESIGN §2.1: a batch is one transaction, gets one config_version
// increment, and only successful commands are part of that version.
// Failures are reported but do not abort the batch.
type BatchResult struct {
	// Lang is the interface language this batch was executed in, so
	// FormatReport can render without needing the request context back.
	Lang i18n.Lang

	// NewVersion is the config_version after this batch.
	// Equals OldVersion if all commands failed (no version bump).
	NewVersion int64
	OldVersion int64

	// Per-command outcomes, in input order.
	Outcomes []CommandOutcome

	// Aggregated effects across all successful commands.
	AggregatedEffects Effects

	// PendingConfirmations are commands held back awaiting /confirm.
	// They are NOT part of the new version.
	PendingConfirmations []PendingConfirmation
}

// CommandOutcome reports the result of one command in a batch.
type CommandOutcome struct {
	Index   int // 0-based position in the batch
	Command *Command
	Effects Effects
	Err     error // nil on success
}

// AnyFailed reports whether any outcome in the batch failed.
func (br *BatchResult) AnyFailed() bool {
	for _, o := range br.Outcomes {
		if o.Err != nil {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────
// Pending confirmations (the /confirm flow, in-memory)
// ─────────────────────────────────────────────────────────────────────

// PendingConfirmation represents a dangerous command held back awaiting
// explicit /confirm from the user.
//
// Storage is in-memory ONLY (per V4-DESIGN philosophy: process restart
// drops pending confirmations; user re-issues if still wanted). A
// background goroutine sweeps expired entries every minute.
//
// Currently triggered by:
//   - port-protocol redefinition (V4-DESIGN §8 port-protocol global rule)
//   - cascade delete of a rule object that has live bind references
type PendingConfirmation struct {
	ID        string   // short random token shown to user; user replies "/confirm <ID>"
	Command   *Command // the command waiting to execute
	Reason    string   // human-readable explanation ("port 443 is bound to https...")
	IssuedAt  time.Time
	ExpiresAt time.Time // typically IssuedAt + 5 minutes
	IssuedBy  string    // optional: telegram user id, for audit
}

// PendingConfirmReason is a typed enum of why a confirmation is needed.
// Used so handlers can request specific UX behavior.
type PendingConfirmReason string

const (
	ReasonPortProtocolConflict PendingConfirmReason = "port_protocol_conflict"
	ReasonCascadeDelete        PendingConfirmReason = "cascade_delete"
)

// ─────────────────────────────────────────────────────────────────────
// Errors
// ─────────────────────────────────────────────────────────────────────

// All command-system errors satisfy this interface so the bot/cli layer
// can format them consistently.
type CommandError interface {
	error
	Code() string // short stable identifier, e.g. "BAD_PARAMS"
}

type cmdErr struct {
	code string
	msg  string
}

func (e *cmdErr) Error() string { return e.msg }
func (e *cmdErr) Code() string  { return e.code }

func NewError(code, msg string) CommandError {
	return &cmdErr{code: code, msg: msg}
}

// Common error codes. Add to this list rather than inventing new strings.
const (
	ErrBadFormat        = "BAD_FORMAT"       // command shape wrong
	ErrUnknownType      = "UNKNOWN_TYPE"     // type not in registry
	ErrBadParams        = "BAD_PARAMS"       // params failed Validate
	ErrNotFound         = "NOT_FOUND"        // /d on nonexistent rule (informational, not always fatal)
	ErrConfirmRequired  = "CONFIRM_REQUIRED" // command held pending /confirm
	ErrConfirmExpired   = "CONFIRM_EXPIRED"  // /confirm <ID> arrived too late
	ErrConfirmUnknownID = "CONFIRM_UNKNOWN"  // /confirm <ID> for an unknown ID
	ErrPortConflict     = "PORT_CONFLICT"    // port-protocol clash without confirm
	ErrCascadeBlocked   = "CASCADE_BLOCKED"  // rule object has live bind references
	ErrInternal         = "INTERNAL"         // bug, not user-fault
)

// ─────────────────────────────────────────────────────────────────────
// Effects merging (used by batch executor)
// ─────────────────────────────────────────────────────────────────────

// Merge combines two Effects into the receiver. Used by the batch executor
// to accumulate effects across all successful commands in a batch.
//
// Merge is associative and commutative for the boolean/list fields, so
// the order of merge calls does not matter for nginx/cert/event semantics.
// UserMessage is appended with a separator.
func (e *Effects) Merge(other Effects) {
	if other.NeedsNginxReload {
		e.NeedsNginxReload = true
	}
	if other.ForceVersionBump {
		e.ForceVersionBump = true
	}
	if other.PurgeCache {
		e.PurgeCache = true
	}
	if other.PurgeCacheBroadcast {
		e.PurgeCacheBroadcast = true
	}
	e.NeedsCertReselect = append(e.NeedsCertReselect, other.NeedsCertReselect...)
	e.EventNotifications = append(e.EventNotifications, other.EventNotifications...)
	e.PortProtocolBindings = append(e.PortProtocolBindings, other.PortProtocolBindings...)
	if other.UserMessage != "" {
		if e.UserMessage != "" {
			e.UserMessage += "\n"
		}
		e.UserMessage += other.UserMessage
	}
	// FileAttachment: last write wins. In practice only one command per
	// batch produces a file (export), so this is unambiguous.
	if other.FileAttachment != nil {
		e.FileAttachment = other.FileAttachment
	}
}

// ─────────────────────────────────────────────────────────────────────
// Param parsing helper (used by handlers for key=value style params)
// ─────────────────────────────────────────────────────────────────────

// ParseKeyValueParams turns a "key1=val1 key2=val2,val3" string into a map.
// Per V4-DESIGN §8 cache/defense object format.
//
// Values containing commas are returned as the raw string; handlers split
// further as appropriate.
//
// Example:
//
//	patterns=*.jpg,*.png ttl=604800 hsts=true
//	→ {"patterns": "*.jpg,*.png", "ttl": "604800", "hsts": "true"}
//
// The placeholder "-" returns an empty map (no params).
func ParseKeyValueParams(s string) (map[string]string, error) {
	if IsPlaceholder(s) || s == "" {
		return map[string]string{}, nil
	}
	out := make(map[string]string)
	for _, tok := range strings.Fields(s) {
		eq := strings.IndexByte(tok, '=')
		if eq <= 0 {
			return nil, NewError(ErrBadParams, fmt.Sprintf("expected key=value, got %q", tok))
		}
		key := tok[:eq]
		val := tok[eq+1:]
		if _, dup := out[key]; dup {
			return nil, NewError(ErrBadParams, fmt.Sprintf("duplicate key %q", key))
		}
		out[key] = val
	}
	return out, nil
}
