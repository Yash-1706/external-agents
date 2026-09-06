package redact

import (
	"reflect"
	"strings"
	"testing"
)

// TestTextRedactsCredentials covers every shape plan §34 names, one row per
// shape. The expected strings are written out in full rather than asserted with
// "contains", because the exact surviving text is part of the contract: the
// reader must still learn *what kind of* secret was removed and where.
func TestTextRedactsCredentials(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "aws access key id",
			in:   "deploy failed using AKIAIOSFODNN7EXAMPLE as the caller",
			want: "deploy failed using [REDACTED] as the caller",
		},
		{
			name: "aws sts session key",
			in:   "assumed role key ASIAY34FZKBOKMUTVV7A expired",
			want: "assumed role key [REDACTED] expired",
		},
		{
			name: "bare bearer token",
			in:   "retried with Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0 and got 200",
			want: "retried with Bearer [REDACTED] and got 200",
		},
		{
			name: "authorization header takes the whole value",
			in:   `curl -H "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abc" https://api.example.com/pause`,
			want: `curl -H "Authorization: [REDACTED]" https://api.example.com/pause`,
		},
		{
			name: "authorization basic header",
			in:   "Authorization: Basic dXNlcjpodW50ZXIy",
			want: "Authorization: [REDACTED]",
		},
		{
			name: "openai style token",
			in:   "OPENAI key sk-proj-4f9a2b7c1d8e6f0a3b5c7d9e1f2a3b4c rejected",
			want: "OPENAI key [REDACTED] rejected",
		},
		{
			name: "github personal access token",
			in:   "push rejected for ghp_0123456789abcdefghijklmnopqrstuvwxyz",
			want: "push rejected for [REDACTED]",
		},
		{
			name: "github server token",
			in:   "actions used ghs_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
			want: "actions used [REDACTED]",
		},
		{
			name: "slack bot token",
			in:   "notify hook uses xoxb-2154-1234567890-abcDEF123456",
			want: "notify hook uses [REDACTED]",
		},
		{
			name: "key equals value in shell export",
			in:   "exported AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCY",
			want: "exported AWS_SECRET_ACCESS_KEY=[REDACTED]",
		},
		{
			name: "key colon value keeps the key visible",
			in:   "password: hunter2",
			want: "password: [REDACTED]",
		},
		{
			name: "json value keeps its quotes so the line stays parseable",
			in:   `{"api_key":"abc123","tool":"run_tests"}`,
			want: `{"api_key":"[REDACTED]","tool":"run_tests"}`,
		},
		{
			name: "yaml client secret with underscore prefix",
			in:   "client_secret: 8f14e45fceea167a5a36dedd4bea2543",
			want: "client_secret: [REDACTED]",
		},
		{
			name: "header style hyphenated key",
			in:   "X-Api-Key: 6f1d8e2b4c9a0f37",
			want: "X-Api-Key: [REDACTED]",
		},
		{
			name: "single quoted value",
			in:   "token='ghu_abcdefghijklmnop', retrying",
			want: "token='[REDACTED]', retrying",
		},
		{
			name: "url userinfo keeps scheme and host as evidence",
			in:   "cloning https://deploy:s3cr3tP4ss@github.com/acme/billing.git",
			want: "cloning https://[REDACTED]@github.com/acme/billing.git",
		},
		{
			name: "postgres url userinfo",
			in:   "DSN postgres://billing:pa55w0rd@db.internal:5432/billing",
			want: "DSN postgres://[REDACTED]@db.internal:5432/billing",
		},
		{
			name: "pem private key block",
			in:   "captured key:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\nq3f9\n-----END RSA PRIVATE KEY-----\nend of log",
			want: "captured key:\n[REDACTED]\nend of log",
		},
		{
			name: "truncated pem block with no footer",
			in:   "payload was -----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIB2",
			want: "payload was [REDACTED]",
		},
		{
			name: "multiple credentials in one line",
			in:   "AKIAIOSFODNN7EXAMPLE and ghp_0123456789abcdefghijklmnopqrstuvwxyz both leaked",
			want: "[REDACTED] and [REDACTED] both leaked",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Text(tc.in); got != tc.want {
				t.Errorf("Text(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTextLeavesOrdinaryProseAlone is the other half of the contract. The
// package is allowed to over-redact a credential-shaped line, but it must not
// shred normal engineering prose: a handoff whose every sentence ends in
// [REDACTED] tells the next worker nothing.
func TestTextLeavesOrdinaryProseAlone(t *testing.T) {
	prose := []string{
		"The auth flow rejects a paused subscription before it reaches billing.",
		"Rotate the password before the demo and note it in the runbook.",
		"The API key lives in the vault; this repo never reads it directly.",
		"Authentication middleware is covered by TestPauseRequiresSession.",
		"Two of three billing tests still fail after the proration fix.",
		"We rejected the token bucket approach because it hid the retry storm.",
		"See internal/redact/redact.go for the secret handling rules.",
		"go test ./billing/... failed: TestPauseResumesBilling",
		"exit_code=1 duration=4.2s package=billing",
		"Checkpoint ck_pre_noon is the last verified state.",
		"git@github.com:acme/billing.git is the remote.",
		"Credentials are never persisted; that is the whole point.",
	}
	for _, s := range prose {
		t.Run(s[:min(len(s), 40)], func(t *testing.T) {
			if got := Text(s); got != s {
				t.Errorf("prose was altered\n  in: %q\n out: %q", s, got)
			}
		})
	}
}

// TestTextRedactsBracketedAndStructuredValues is a regression test for a hole in
// the key/value rule: the value class used to exclude '[' and ']' as a cheap way
// of making the placeholder unmatchable on a second pass, which meant a value
// written in brackets matched as *empty* and the credential survived untouched.
// A structured value is now taken whole, so nothing inside it can be left behind.
func TestTextRedactsBracketedAndStructuredValues(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bracketed value is a value, not an empty match",
			in:   "api_key: [s3cr3tV4lue]",
			want: "api_key: [REDACTED]",
		},
		{
			name: "nested object value is taken whole",
			in:   `{"api_key": {"value":"s3cr3tV4lue"}}`,
			want: `{"api_key": [REDACTED]}`,
		},
		{
			name: "array value is taken whole",
			in:   `{"credentials": ["s3cr3tV4lue","other"]}`,
			want: `{"credentials": [REDACTED]}`,
		},
		{
			name: "deeply nested credential cannot hide behind an outer key",
			in:   `{"auth": {"basic": {"password":"s3cr3tV4lue"}}}`,
			want: `{"auth": [REDACTED]}`,
		},
		{
			name: "a brace inside a string does not end the structure",
			in:   `{"api_key": {"note":"a } brace","v":"s3cr3tV4lue"}}`,
			want: `{"api_key": [REDACTED]}`,
		},
		{
			name: "the structure ends where it ends and the rest of the line survives",
			in:   `{"api_key": {"v":"s3cr3tV4lue"}, "tool":"run_tests"}`,
			want: `{"api_key": [REDACTED], "tool":"run_tests"}`,
		},
		{
			name: "an unbalanced structure falls back to the line, not the whole input",
			in:   "api_key: {s3cr3tV4lue\nTestPauseResumesBilling failed",
			want: "api_key: [REDACTED]\nTestPauseResumesBilling failed",
		},
		{
			name: "shell assignment of a json blob",
			in:   `secret={"a":"s3cr3tV4lue"}`,
			want: `secret=[REDACTED]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Text(tc.in); got != tc.want {
				t.Errorf("Text(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
			if strings.Contains(Text(tc.in), "s3cr3tV4lue") {
				t.Errorf("Text(%q) let the credential through", tc.in)
			}
		})
	}
}

// TestTextRedactsURLUserinfoWithoutAUsername is a regression test for DSNs whose
// userinfo is password-only. "redis://:pw@host" and "amqp://:pw@host" are the
// ordinary spellings for those services, and a rule that required a username let
// the password through in exactly the URLs most likely to be pasted into a
// summary.
func TestTextRedactsURLUserinfoWithoutAUsername(t *testing.T) {
	redacted := []struct{ in, want string }{
		{"cache DSN redis://:s3cr3tV4lue@cache:6379/0", "cache DSN redis://[REDACTED]@cache:6379/0"},
		{"amqp://:s3cr3tV4lue@broker/vhost", "amqp://[REDACTED]@broker/vhost"},
		{"redis://user:s3cr3tV4lue@cache:6379", "redis://[REDACTED]@cache:6379"},
	}
	for _, tc := range redacted {
		if got := Text(tc.in); got != tc.want {
			t.Errorf("Text(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
		}
	}
	// The looser username must not start matching ordinary URLs that merely
	// contain a colon: a host:port or a path segment is not userinfo.
	untouched := []string{
		"https://api.example.com/v1/pause",
		"jdbc:postgresql://db.internal:5432/billing",
		"see https://docs.example.com/a:b for the pause contract",
		"git@github.com:acme/billing.git is the remote.",
	}
	for _, s := range untouched {
		if got := Text(s); got != s {
			t.Errorf("url without userinfo was altered\n  in: %q\n out: %q", s, got)
		}
	}
}

// TestTextIsIdempotent guards determinism: a summary is redacted by the adapter
// and may be redacted again when state is merged and re-rendered. If the second
// pass changed the string, two runs over the same input would diverge.
func TestTextIsIdempotent(t *testing.T) {
	inputs := []string{
		`{"api_key":"abc123"}`,
		"password: hunter2",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abc",
		"AKIAIOSFODNN7EXAMPLE",
		"https://deploy:s3cr3t@github.com/acme/billing.git",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
		"nothing sensitive here at all",
		// The shapes whose idempotence used to be bought by excluding brackets
		// from the value class. It is now bought by recognising the placeholder,
		// so these must stay stable on their own.
		"api_key: [s3cr3t]",
		`{"api_key": {"value":"s3cr3t"}}`,
		"token: [REDACTED]",
		`{"api_key": [REDACTED]}`,
		"password: https://u:p@h/x",
		"redis://:s3cr3t@cache:6379",
		"token=abc123, exit_code=1",
	}
	for _, in := range inputs {
		once := Text(in)
		twice := Text(once)
		if once != twice {
			t.Errorf("Text is not idempotent for %q:\n first: %q\nsecond: %q", in, once, twice)
		}
	}
}

// TestTextOverRedactionIsBounded pins the documented tradeoff in place. These
// inputs lose more than the secret itself, and that is the accepted behaviour —
// but the loss must stop at the end of the value, never run past a quote, a
// comma or the end of the line into unrelated content.
func TestTextOverRedactionIsBounded(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "prose after a credential key is sacrificed",
			in:   "password: hunter2 was still in the env file",
			want: "password: [REDACTED]",
		},
		{
			name: "redaction stops at the comma",
			in:   "token=abc123, exit_code=1",
			want: "token=[REDACTED], exit_code=1",
		},
		{
			name: "redaction stops at the closing brace",
			in:   "{secret: shh} and the rest of the line survives",
			want: "{secret: [REDACTED]} and the rest of the line survives",
		},
		{
			name: "redaction never crosses a newline",
			in:   "secret:\nThe next paragraph is unrelated and must survive.",
			want: "secret:\nThe next paragraph is unrelated and must survive.",
		},
		{
			name: "only the matched line is affected",
			in:   "api_key=abc123\nTestPauseResumesBilling failed",
			want: "api_key=[REDACTED]\nTestPauseResumesBilling failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Text(tc.in); got != tc.want {
				t.Errorf("Text(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMap(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{
			name: "nil map stays nil",
			in:   nil,
			want: nil,
		},
		{
			name: "empty map stays empty",
			in:   map[string]string{},
			want: map[string]string{},
		},
		{
			name: "sensitive key redacts the whole value even when it looks harmless",
			in:   map[string]string{"api_key": "abc", "token": "x", "authorization": "Basic dXNlcg=="},
			want: map[string]string{"api_key": "[REDACTED]", "token": "[REDACTED]", "authorization": "[REDACTED]"},
		},
		{
			name: "real world key spellings",
			in: map[string]string{
				"AWS_SECRET_ACCESS_KEY": "wJalrXUtnFEMI",
				"client_secret":         "8f14e45f",
				"db_password":           "hunter2",
				"private-key":           "MIIEow",
				"refresh_token":         "1//0gabc",
			},
			want: map[string]string{
				"AWS_SECRET_ACCESS_KEY": "[REDACTED]",
				"client_secret":         "[REDACTED]",
				"db_password":           "[REDACTED]",
				"private-key":           "[REDACTED]",
				"refresh_token":         "[REDACTED]",
			},
		},
		{
			name: "ordinary keys keep their values but the value is still scanned",
			in: map[string]string{
				"tool_name":  "run_tests",
				"exit_code":  "1",
				"command":    "aws s3 ls --profile AKIAIOSFODNN7EXAMPLE",
				"repo":       "acme/billing",
				"checkpoint": "ck_pre_noon",
			},
			want: map[string]string{
				"tool_name":  "run_tests",
				"exit_code":  "1",
				"command":    "aws s3 ls --profile [REDACTED]",
				"repo":       "acme/billing",
				"checkpoint": "ck_pre_noon",
			},
		},
		{
			name: "empty sensitive value is not claimed to have held a secret",
			in:   map[string]string{"token": "", "api_key": "   "},
			want: map[string]string{"token": "", "api_key": "   "},
		},
		{
			name: "keys are never rewritten so attributes cannot collide",
			in:   map[string]string{"token": "a", "auth_token": "b"},
			want: map[string]string{"token": "[REDACTED]", "auth_token": "[REDACTED]"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Map(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Map()\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

func TestMapDoesNotMutateInput(t *testing.T) {
	in := map[string]string{"api_key": "abc123"}
	_ = Map(in)
	if in["api_key"] != "abc123" {
		t.Fatalf("Map mutated its input: %#v", in)
	}
}

func TestStrings(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil slice stays nil", in: nil, want: nil},
		{name: "empty slice stays empty", in: []string{}, want: []string{}},
		{
			name: "each element is redacted independently",
			in: []string{
				"git checkout -b feature/subscription-pause",
				"export GITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuvwxyz",
				"go test ./billing/...",
			},
			want: []string{
				"git checkout -b feature/subscription-pause",
				"export GITHUB_TOKEN=[REDACTED]",
				"go test ./billing/...",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Strings(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Strings()\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

func TestStringsDoesNotMutateInput(t *testing.T) {
	in := []string{"password: hunter2"}
	_ = Strings(in)
	if in[0] != "password: hunter2" {
		t.Fatalf("Strings mutated its input: %#v", in)
	}
}

// TestTextHandlesTranscriptShapedInput exercises a realistic multi-line blob of
// the kind a hook shim might hand us, to prove the rules compose rather than
// fighting each other.
func TestTextHandlesTranscriptShapedInput(t *testing.T) {
	in := strings.Join([]string{
		"$ terraform apply",
		`provider config: {"region":"us-east-1","access_key":"AKIAIOSFODNN7EXAMPLE"}`,
		"remote: https://ci:d3ploy@github.com/acme/billing.git",
		"Error: 2 of 3 billing tests still fail",
	}, "\n")
	want := strings.Join([]string{
		"$ terraform apply",
		`provider config: {"region":"us-east-1","access_key":"[REDACTED]"}`,
		"remote: https://[REDACTED]@github.com/acme/billing.git",
		"Error: 2 of 3 billing tests still fail",
	}, "\n")
	if got := Text(in); got != want {
		t.Errorf("Text()\n got: %q\nwant: %q", got, want)
	}
}
