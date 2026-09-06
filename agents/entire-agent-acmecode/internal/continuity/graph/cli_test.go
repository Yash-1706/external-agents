package graph

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// stubCLI builds a client whose shell-out is replaced, so command construction
// and error mapping are covered without an Entire installation.
func stubCLI(out string, runErr error, got *[]string) *cliClient {
	return &cliClient{
		bin: "entire",
		dir: ".",
		run: func(_ context.Context, args ...string) ([]byte, error) {
			if got != nil {
				*got = args
			}
			if runErr != nil {
				return nil, runErr
			}
			return []byte(out), nil
		},
		lookPath: func(string) (string, error) { return "/usr/bin/entire", nil },
	}
}

func TestCLICommandConstruction(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		call func(c *cliClient) error
		want []string
	}{
		{
			name: "search",
			call: func(c *cliClient) error { _, err := c.Search(ctx, "Process"); return err },
			want: []string{"graph", "search", "--json", "Process"},
		},
		{
			name: "impact",
			call: func(c *cliClient) error { _, err := c.Impact(ctx, "Process"); return err },
			want: []string{"graph", "impact", "--json", "Process"},
		},
		{
			name: "semantic diff",
			call: func(c *cliClient) error { _, err := c.SemanticDiff(ctx, "CP2", "HEAD"); return err },
			want: []string{"graph", "semantic-diff", "--json", "CP2", "HEAD"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			// A payload each decoder accepts, so only the arguments are asserted.
			c := stubCLI(`{"target":{"name":"Process","path":"a.go"}}`, nil, &got)
			if err := tc.call(c); err != nil {
				t.Fatalf("call error = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("args = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCLIAvailable: probing must be a PATH lookup, never a command execution.
func TestCLIAvailable(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		bin  string
		look func(string) (string, error)
		want bool
	}{
		{name: "resolves", bin: "entire", look: func(string) (string, error) { return "/usr/bin/entire", nil }, want: true},
		{name: "not on path", bin: "entire", look: func(string) (string, error) { return "", errors.New("not found") }, want: false},
		{name: "no binary configured", bin: "", look: func(string) (string, error) { return "x", nil }, want: false},
		{name: "no lookup wired", bin: "entire", look: nil, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &cliClient{bin: tc.bin, lookPath: tc.look}
			if got := c.Available(ctx); got != tc.want {
				t.Errorf("Available() = %v, want %v", got, tc.want)
			}
		})
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	c := &cliClient{bin: "entire", lookPath: func(string) (string, error) { return "x", nil }}
	if c.Available(cancelled) {
		t.Error("Available reported true under a cancelled context")
	}
}

// TestCLIPropagatesUnavailability: a failed shell-out must surface as
// ErrUnavailable, so callers degrade instead of reading an empty result as an
// empty blast radius (plan §48).
func TestCLIPropagatesUnavailability(t *testing.T) {
	ctx := context.Background()
	runErr := errors.New("capability unavailable: entire graph search failed: exit status 127")
	c := stubCLI("", runErr, nil)

	if _, err := c.Search(ctx, "Process"); !errors.Is(err, runErr) {
		t.Errorf("Search error = %v, want the run error", err)
	}
	if _, err := c.Impact(ctx, "Process"); !errors.Is(err, runErr) {
		t.Errorf("Impact error = %v, want the run error", err)
	}
	if _, err := c.SemanticDiff(ctx, "a", "b"); !errors.Is(err, runErr) {
		t.Errorf("SemanticDiff error = %v, want the run error", err)
	}
}

func TestCLISortsResults(t *testing.T) {
	ctx := context.Background()
	payload := `[{"name":"Zed","path":"z.go","line_start":3},{"name":"Amy","path":"a.go","line_start":9},{"name":"Ann","path":"a.go","line_start":2}]`
	c := stubCLI(payload, nil, nil)
	got, err := c.Search(ctx, "x")
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	want := []string{"a.go:Ann", "a.go:Amy", "z.go:Zed"}
	if !reflect.DeepEqual(names(got), want) {
		t.Errorf("Search = %v, want %v (sorted by location)", names(got), want)
	}
}

func TestCLIImpactIsNormalised(t *testing.T) {
	payload := `{"target":{"name":"Process","path":"billing/webhook.go"},
	             "callers":[{"name":"Run","path":"retry/scheduler.go","line_start":17}],
	             "dependents":["retry","BillingService","retry"],
	             "tests":["TestRetry","TestBillingResume"],
	             "files":["retry/scheduler.go","billing/webhook.go"]}`
	imp, err := stubCLI(payload, nil, nil).Impact(context.Background(), "Process")
	if err != nil {
		t.Fatalf("Impact error = %v", err)
	}
	if want := []string{"BillingService", "retry"}; !reflect.DeepEqual(imp.Dependents, want) {
		t.Errorf("dependents = %v, want %v (sorted, deduped)", imp.Dependents, want)
	}
	if want := []string{"TestBillingResume", "TestRetry"}; !reflect.DeepEqual(imp.Tests, want) {
		t.Errorf("tests = %v, want %v", imp.Tests, want)
	}
	if want := []string{"billing/webhook.go", "retry/scheduler.go"}; !reflect.DeepEqual(imp.Files, want) {
		t.Errorf("files = %v, want %v", imp.Files, want)
	}
}

func TestDecodeSymbols(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "bare array", raw: `[{"name":"Process","path":"a.go"}]`, want: []string{"a.go:Process"}},
		{name: "wrapped", raw: `{"symbols":[{"name":"Process","path":"a.go"}]}`, want: []string{"a.go:Process"}},
		{name: "empty object", raw: `{}`, want: []string{}},
		{name: "empty array", raw: `[]`, want: []string{}},
		{name: "blank output means no matches", raw: "  \n", want: []string{}},
		{name: "malformed is unavailable, not empty", raw: `{"symbols": oops}`, wantErr: true},
		{name: "wrong shape is unavailable", raw: `"Process"`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeSymbols([]byte(tc.raw))
			if tc.wantErr {
				if !errors.Is(err, model.ErrUnavailable) {
					t.Fatalf("error = %v, want ErrUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if !reflect.DeepEqual(names(got), tc.want) {
				t.Errorf("symbols = %v, want %v", names(got), tc.want)
			}
		})
	}
}

func TestDecodeImpact(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "object", raw: `{"target":{"name":"Process","path":"a.go"}}`},
		{
			// Silence is not "nothing depends on this symbol".
			name:    "blank output is unavailable",
			raw:     "   ",
			wantErr: true,
		},
		{name: "malformed", raw: `{`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeImpact([]byte(tc.raw))
			if tc.wantErr != errors.Is(err, model.ErrUnavailable) {
				t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestDecodeChanges(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{name: "bare array", raw: `[{"symbol":"Process","change":"modified"}]`, want: 1},
		{name: "wrapped", raw: `{"changes":[{"symbol":"Process","change":"added"}]}`, want: 1},
		{name: "blank means no semantic change", raw: "", want: 0},
		{name: "empty object", raw: `{}`, want: 0},
		{name: "malformed", raw: `[{`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeChanges([]byte(tc.raw))
			if tc.wantErr {
				if !errors.Is(err, model.ErrUnavailable) {
					t.Fatalf("error = %v, want ErrUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("changes = %d, want %d", len(got), tc.want)
			}
			if got == nil {
				t.Error("changes must be non-nil so output carries [] not null")
			}
		})
	}
}

func TestCLIDescribeNamesTheBackend(t *testing.T) {
	d := NewCLI("entire", "/repo").Describe()
	for _, want := range []string{"entire graph CLI", "entire", "/repo"} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe() = %q, want it to contain %q", d, want)
		}
	}
}
