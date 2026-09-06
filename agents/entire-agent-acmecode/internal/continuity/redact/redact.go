// Package redact removes credential-shaped content from any text the continuity
// layer is about to persist, render, or hand to a model.
//
// Plan §34 is unconditional: the product must not create a new secret leak path.
// Continuity reads other people's sessions and writes durable checkpoints, so a
// credential that reaches an AgentEvent summary or an attribute bag is a
// credential written to disk and replayed into a future agent's context. Every
// adapter therefore funnels its free text through Text and its attribute maps
// through Map before an event is emitted.
//
// # The deliberate tradeoff
//
// This package is biased towards over-redaction, and that bias is a product
// decision, not an accident. The rules that match a credential-shaped key
// ("password:", "api_key=", "Authorization:") redact the whole rest of the value
// — up to the closing quote, comma or line end, or, when the value opens an
// object or an array, up to its matching close — without trying to decide
// whether what follows is a secret or prose. That means an ordinary sentence
// like "password: rotate it quarterly" loses its tail.
//
// We accept that because the two failure modes are not symmetric. Over-redaction
// costs a reader one sentence of a summary, and the underlying artifact is still
// reachable through the event's PayloadRef. Under-redaction writes a live
// credential into a checkpoint that outlives the session, gets handed to another
// agent, and cannot be recalled. Losing a sentence is recoverable; leaking a key
// is not.
//
// The bias is bounded, though: rules only fire on credential *shapes*. Bare
// nouns in prose ("the auth flow", "the API key is in the vault", "rotate the
// password") carry no separator and no token-shaped payload, so they survive
// untouched. See the false-positive table in redact_test.go for the contract.
package redact

import (
	"regexp"
	"strings"
)

// placeholder replaces every redacted span. It is intentionally a visible,
// greppable marker rather than an empty string: a reader must be able to tell
// "a secret was here and was removed" from "nothing was here", which is the same
// honesty rule the rest of the system follows about missing context (plan §33).
const placeholder = "[REDACTED]"

// sensitiveKey matches an identifier that names a credential. The surrounding
// [A-Za-z0-9_.-]* runs are what let it catch the real-world spellings —
// client_secret, AWS_SECRET_ACCESS_KEY, X-Api-Key, auth_token, refresh_token —
// rather than only the bare word.
const sensitiveKey = `[A-Za-z0-9_.\-]*(?:api[_-]?key|secret|token|password|passwd|credential|private[_-]?key|auth)[A-Za-z0-9_.\-]*`

// valuePattern describes the value that follows a sensitive key.
//
// The quoted alternatives come first so that a redacted JSON, YAML or shell line
// stays syntactically intact. The bare alternative deliberately allows spaces —
// "Authorization: Bearer eyJ..." is one value, not two — but stops at the
// characters that end a value in every format we actually see: quotes, comma,
// semicolon, and the closing brace/paren. Credentials in the wild (base64,
// base64url, hex, JWT) never contain those, so stopping there cannot truncate a
// secret and leave half of it behind.
//
// Square brackets are NOT excluded. An earlier version excluded them so that the
// bare alternative could never re-match the "[REDACTED]" it had just written,
// which bought idempotence at the price of a hole: "api_key: [s3cr3t]" matched an
// empty value and the credential survived untouched. Idempotence is now enforced
// where it belongs — redactKeyValues skips a value that already *is* the
// placeholder — so the value class can stay honest about what a value looks like.
const valuePattern = `"[^"\r\n]*"|'[^'\r\n]*'|` + "`[^`\r\n]*`" + `|[^\r\n"'` + "`" + `,;}\)]*`

var (
	// pemBlock matches a complete PEM private key. RE2 has no backreferences, so
	// the header/footer key type is matched loosely rather than pinned to be
	// equal; a stray "BEGIN RSA ... END EC" pair is redacted either way, which is
	// the safe direction.
	pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

	// pemOpen catches a private key whose footer never arrived — a truncated log
	// line, a clipped tool payload. A BEGIN header with no END is still key
	// material, so everything after it goes.
	pemOpen = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*`)

	// urlCred matches the user:password@ form of URL userinfo. The scheme and
	// host survive: they are evidence (which registry, which host was reached)
	// and they are not the secret. Only the userinfo is replaced.
	//
	// The user half is [^\s/@:]* rather than + because the username is optional in
	// practice: "redis://:s3cr3t@cache:6379" and "amqp://:s3cr3t@broker/" are the
	// ordinary spellings for those services, and requiring a username let the
	// password through in exactly the URLs most likely to appear in a DSN.
	urlCred = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\s/@:]*:[^\s/@]*@`)

	// keyValue matches "<credential-shaped key><separator><value>" in one line.
	// The optional quote in the separator is what lets a JSON key ("api_key":)
	// match as well as a bare one (api_key=). The separator run is [ \t]* rather
	// than \s* on purpose: allowing it to cross a newline would let a whole prose
	// paragraph following a line that merely ends in "secret:" be eaten.
	keyValue = regexp.MustCompile(`(?i)\b(` + sensitiveKey + `)(["']?[ \t]*[:=][ \t]*)(` + valuePattern + `)`)

	// bearer catches a bearer token that is not preceded by a key, e.g. a token
	// quoted inline in a log message. The word "Bearer" is kept so the reader can
	// still see what kind of credential was removed. The {8,} floor keeps short
	// prose ("bearer of bad news") out.
	bearer = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=\-]{8,}`)

	// awsKey matches an AWS access key id: AKIA (long-lived) or ASIA (STS) plus
	// exactly 16 uppercase alphanumerics. This shape is unambiguous, so it is
	// matched case-sensitively and needs no surrounding context.
	awsKey = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)

	// prefixToken matches vendor tokens that announce themselves with a prefix:
	// OpenAI (sk-), GitHub personal/OAuth/user/server (ghp_ gho_ ghu_ ghs_) and
	// Slack (xoxb- xoxa- xoxp- xoxr- xoxs-). The length floors keep ordinary
	// hyphenated words from matching.
	prefixToken = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_\-]{16,}|gh[pous]_[A-Za-z0-9]{16,}|xox[baprs]-[A-Za-z0-9\-]{8,})`)

	// sensitiveKeyOnly is the whole-string form used by Map: it asks whether an
	// attribute *name* announces that its value is a credential.
	sensitiveKeyOnly = regexp.MustCompile(`(?i)^` + sensitiveKey + `$`)
)

// Text returns s with every credential-shaped span replaced by "[REDACTED]".
//
// It is safe to call on text that has already been redacted: every rule either
// cannot match the placeholder it wrote (the token rules) or recognises it and
// declines to rewrite it (the key/value rule), so Text(Text(s)) == Text(s). That matters
// because a summary may pass through here once in an adapter and again when a
// state is merged and re-rendered, and a value that changed on the second pass
// would break the determinism the rest of the system depends on.
func Text(s string) string {
	if s == "" {
		return s
	}
	// Order is load-bearing. Whole-block and structural rules run before
	// token-shaped ones so that the broadest correct redaction wins: an
	// "Authorization: Bearer <jwt>" line is redacted as one header value rather
	// than being chopped into a surviving "Bearer" and a redacted tail.
	s = pemBlock.ReplaceAllString(s, placeholder)
	s = pemOpen.ReplaceAllString(s, placeholder)
	s = urlCred.ReplaceAllString(s, "${1}"+placeholder+"@")
	s = redactKeyValues(s)
	s = bearer.ReplaceAllString(s, "Bearer "+placeholder)
	s = awsKey.ReplaceAllString(s, placeholder)
	s = prefixToken.ReplaceAllString(s, placeholder)
	return s
}

// Map returns a copy of m with every value redacted. A value is replaced
// wholesale when its *key* names a credential (api_key, aws_secret_access_key,
// authorization); otherwise the value is passed through Text.
//
// Keys are never rewritten. They are the attribute vocabulary the rest of the
// system indexes on — Attr("tool_name"), Attr("checkpoint_id") — and redacting a
// key would either silently drop an attribute or collide two attributes into
// one. A key is a name, not a payload; §34 is about payloads.
//
// A nil map returns nil so that callers keep the "absent" / "present but empty"
// distinction their omitempty JSON tags rely on. The input map is never mutated.
func Map(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		// Iterating a map is unordered, but each entry is redacted independently
		// and the result is itself a map, so no ordering can reach the output.
		if sensitiveKeyOnly.MatchString(strings.TrimSpace(k)) {
			if strings.TrimSpace(v) == "" {
				// An empty value holds no secret; claiming one was removed would
				// be its own small fabrication.
				out[k] = v
				continue
			}
			out[k] = placeholder
			continue
		}
		out[k] = Text(v)
	}
	return out
}

// Strings returns a copy of in with every element redacted. A nil slice returns
// nil; the input slice is never mutated.
func Strings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = Text(s)
	}
	return out
}

// redactKeyValues replaces the value half of every "<sensitive key><sep><value>"
// match, leaving the key and separator in place so the reader can still see
// which credential was present without seeing the credential.
//
// This is written against submatch indices rather than a plain ReplaceAll so it
// can (a) preserve the quoting style of the original value, keeping redacted
// JSON and YAML parseable, and (b) skip a value that is already the placeholder,
// which is what makes Text idempotent.
func redactKeyValues(s string) string {
	matches := keyValue.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, m := range matches {
		valStart, valEnd := m[6], m[7]
		if valStart < 0 || valStart < last {
			// Either the regex captured nothing, or this match lies inside a
			// structured value an earlier iteration already consumed whole.
			continue
		}
		if end, ok := structuredEnd(s, valStart); ok {
			// The value is an object or an array. The regex can only see its first
			// character, so take the whole balanced structure instead: replacing
			// just the "{" would leave the secret inside it in the output.
			valEnd = end
		}
		val := s[valStart:valEnd]
		// Trailing whitespace belongs to the surrounding text, not the value.
		trimmed := strings.TrimRight(val, " \t")
		if trimmed == "" || trimmed == placeholder {
			// Nothing to remove, or already removed on an earlier pass.
			continue
		}
		b.WriteString(s[last:valStart])
		if q := trimmed[0]; len(trimmed) >= 2 && (q == '"' || q == '\'' || q == '`') && trimmed[len(trimmed)-1] == q {
			b.WriteByte(q)
			b.WriteString(placeholder)
			b.WriteByte(q)
		} else {
			b.WriteString(placeholder)
		}
		b.WriteString(val[len(trimmed):])
		last = valEnd
	}
	b.WriteString(s[last:])
	return b.String()
}

// structuredEnd reports the index just past the balanced object or array that
// begins at i, if one does.
//
// A credential-shaped key whose value is a nested structure — {"api_key":{"v":
// "s3cr3t"}} — cannot be handled by the value regex, which stops at the first
// quote and so would redact the opening brace and leave the secret behind it. RE2
// has no recursion, so the structure is walked here instead.
//
// Quoted strings are respected (a brace inside a string is text, not structure)
// and an unbalanced structure reports false, so a stray "{" in prose falls back
// to the regex value rather than swallowing the rest of the input.
func structuredEnd(s string, i int) (int, bool) {
	if i < 0 || i >= len(s) {
		return 0, false
	}
	var opener, closer byte
	switch s[i] {
	case '{':
		opener, closer = '{', '}'
	case '[':
		opener, closer = '[', ']'
	default:
		return 0, false
	}
	depth := 0
	inString := false
	escaped := false
	for j := i; j < len(s); j++ {
		c := s[j]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case opener:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return j + 1, true
			}
		}
	}
	return 0, false
}
