// Package slug turns branch names and other free-form identifiers into
// DNS-safe hostname labels.
package slug

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// MaxLabel is the maximum length of a single DNS label (RFC 1035).
const MaxLabel = 63

// Hash4 returns a short, stable, lowercase hex digest of s.
func Hash4(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:4]
}

// Sanitize converts s into a valid hostname label:
//   - unicode is folded to ASCII where possible (é → e), other runes become '-'
//   - lowercase, anything outside [a-z0-9] becomes '-'
//   - runs of '-' collapse, leading/trailing '-' are trimmed
//   - labels longer than 63 bytes are truncated and suffixed with a hash of
//     the original so distinct long names stay distinct
//   - an empty result becomes "wt-<hash>"
func Sanitize(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range norm.NFKD.String(s) {
		if unicode.Is(unicode.Mn, r) { // combining marks from decomposition
			continue
		}
		r = unicode.ToLower(r)
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "wt-" + Hash4(s)
	}
	if len(out) > MaxLabel {
		out = strings.TrimRight(out[:MaxLabel-5], "-") + "-" + Hash4(s)
	}
	return out
}

// WithSuffix appends a disambiguating hash suffix derived from key while
// keeping the label within MaxLabel.
func WithSuffix(label, key string) string {
	suffix := "-" + Hash4(key)
	if len(label)+len(suffix) > MaxLabel {
		label = strings.TrimRight(label[:MaxLabel-len(suffix)], "-")
	}
	return label + suffix
}

// ValidLabel reports whether s is already a valid, sanitized label.
func ValidLabel(s string) bool {
	return s != "" && Sanitize(s) == s
}

// ValidHostname reports whether h is a syntactically valid hostname made of
// sanitized labels.
func ValidHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if !ValidLabel(l) {
			return false
		}
	}
	return true
}
