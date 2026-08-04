package command

import (
	"context"
	"database/sql"

	"github.com/example/meshcdn/internal/db"
	"github.com/example/meshcdn/internal/i18n"
)

// Language plumbing.
//
// Handlers render their own messages, so they need to know which language to
// render in. Threading it through context — the same shape as WithForce — keeps
// it out of every handler signature and out of the Effects type.
//
// The setting itself lives in cluster_meta and is read once per batch at the
// entry points (bot message, CLI invocation), not per message: it is a config
// read, and a batch should not change language halfway through.

type langKey struct{}

// WithLang returns a context carrying the interface language.
func WithLang(ctx context.Context, lang i18n.Lang) context.Context {
	return context.WithValue(ctx, langKey{}, lang)
}

// LangFrom returns the interface language for this context, or i18n.Default
// when none was injected — so a handler called from a path that never set one
// still produces sensible output rather than an empty string.
func LangFrom(ctx context.Context) i18n.Lang {
	if l, ok := ctx.Value(langKey{}).(i18n.Lang); ok && l.Valid() {
		return l
	}
	return i18n.Default
}

// T is shorthand for i18n.T in the language carried by ctx.
func T(ctx context.Context, key string, args ...interface{}) string {
	return i18n.T(LangFrom(ctx), key, args...)
}

// EffectiveLang reads the configured interface language from the database.
// Falls back to the default when unset or unreadable — the language of the UI
// must never be a reason a command fails.
func EffectiveLang(ctx context.Context, q db.Querier) i18n.Lang {
	stored, err := db.GetLanguage(ctx, q)
	if err != nil {
		return i18n.Default
	}
	if lang, ok := i18n.Parse(stored); ok {
		return lang
	}
	return i18n.Default
}

// ContextWithLangFromDB is the one-call helper used at entry points.
func ContextWithLangFromDB(ctx context.Context, conn *sql.DB) context.Context {
	return WithLang(ctx, EffectiveLang(ctx, conn))
}
