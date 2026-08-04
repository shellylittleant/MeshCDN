// Package i18n renders user-facing text in the operator's chosen language.
//
// Design notes:
//
//   - Chinese is the reference language. Every key must exist in zh; en falls
//     back to zh when a translation is missing, so an untranslated string is a
//     visible gap rather than an empty message or a panic. Catalogue() exposes
//     the tables so a test can assert full coverage.
//
//   - Only text a human reads is translated. Error *codes* (BAD_FORMAT,
//     PORT_CONFLICT, …) are language-neutral by design and stay as they are —
//     that classification was built for exactly this.
//
//   - Command syntax is never translated. `/w domain a.com 443 …` is the same
//     in both languages: per V4-DESIGN §0, the command language is the product
//     and the terminal is a shell over it. Translating the instruction set
//     would fork the product.
package i18n

import (
	"fmt"
	"strings"
)

// Lang is a supported interface language.
type Lang string

const (
	ZH Lang = "cn"
	EN Lang = "en"
)

// Default is used when nothing has been configured.
const Default = ZH

// Parse normalises user input ("EN", "en-US", "zh", "中文") to a Lang.
// Returns ok=false for anything unrecognised so callers can report it rather
// than silently falling back.
func Parse(s string) (Lang, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "cn", "zh", "zh-cn", "zh_cn", "chinese", "中文":
		return ZH, true
	case "en", "en-us", "en_us", "eng", "english":
		return EN, true
	}
	return "", false
}

// Valid reports whether l is a language this build can render.
func (l Lang) Valid() bool { return l == ZH || l == EN }

// String returns the canonical short code.
func (l Lang) String() string { return string(l) }

// DisplayName returns the language's own name, for confirmation messages.
func (l Lang) DisplayName() string {
	if l == EN {
		return "English"
	}
	return "中文"
}

// T looks up key in lang and formats it with args.
//
// Missing keys return a marked-up placeholder ("!!key!!") rather than an empty
// string: a gap in the catalogue should be obvious in the output, not
// invisible.
func T(lang Lang, key string, args ...interface{}) string {
	table := zh
	if lang == EN {
		table = en
	}

	format, ok := table[key]
	if !ok && lang == EN {
		// Untranslated: fall back to Chinese so the message still says
		// something true.
		format, ok = zh[key]
	}
	if !ok {
		return "!!" + key + "!!"
	}
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// Catalogue exposes the message tables for coverage tests.
func Catalogue() (zhTable, enTable map[string]string) { return zh, en }
