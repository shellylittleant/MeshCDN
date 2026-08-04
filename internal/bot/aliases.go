package bot

import "strings"

// Bare-verb aliases for the Telegram terminal.
//
// The command language is strictly four segments and stays that way — Parse()
// rejects anything else, deliberately (V4-DESIGN §8.1). But typing
// `/v sync - -` into a phone to trigger a sync is terrible, and V4-DESIGN §8.3
// already lists `/sync`, `/help`, `/upgrade` and friends as things an operator
// should be able to type bare.
//
// That gap was real: only `/menu` was ever special-cased, so every other bare
// verb in the design doc failed with "command must have 4 segments". This table
// closes it in the right place — the terminal. Expansion happens before the
// text reaches the executor, so the core grammar is untouched and the CLI still
// takes the full form.
//
// Aliases are exact-match only. `/en` is an alias; `/enable-something` is not.
var bareAliases = map[string]string{
	"/en":      "/v lang en -",
	"/cn":      "/v lang cn -",
	"/zh":      "/v lang cn -",
	"/lang":    "/v lang - -",
	"/help":    "/v help - -",
	"/sync":    "/v sync - -",
	"/status":  "/v status - -",
	"/nodes":   "/v nodes - -",
	"/stats":   "/v stats - -",
	"/export":  "/v export - -",
	"/upgrade": "/v upgrade - -",
}

// expandAlias rewrites a bare-verb message into its four-segment form.
//
// Returns the input unchanged when it is not an alias. Handles Telegram's
// `/cmd@botname` suffix, which the client appends in groups.
func expandAlias(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !strings.HasPrefix(trimmed, "/") {
		return text
	}

	// Split off arguments; aliases take none.
	head := trimmed
	if i := strings.IndexAny(trimmed, " \t\n"); i >= 0 {
		head = trimmed[:i]
	}
	rest := strings.TrimSpace(trimmed[len(head):])

	// `/sync@MyBot` → `/sync`
	if at := strings.Index(head, "@"); at > 0 {
		head = head[:at]
	}

	expanded, ok := bareAliases[strings.ToLower(head)]
	if !ok {
		return text
	}
	// A bare alias with trailing arguments is not an alias — `/stats a.com 24h`
	// is a real command the operator means literally. Rebuild it as /v form
	// with the arguments in place rather than silently dropping them.
	if rest != "" {
		verbType := strings.Fields(expanded)
		if len(verbType) >= 2 {
			args := strings.Fields(rest)
			scope := "-"
			params := "-"
			if len(args) >= 1 {
				scope = args[0]
			}
			if len(args) >= 2 {
				params = strings.Join(args[1:], " ")
			}
			return verbType[0] + " " + verbType[1] + " " + scope + " " + params
		}
		return text
	}
	return expanded
}
