package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// cliClient talks to an installed Entire Graph binary.
//
// run and lookPath are function fields rather than direct calls to os/exec so
// that argument construction and error mapping are testable without an Entire
// installation. They are not configuration: NewCLI is the only public way to
// build this client and it always wires the real implementations.
type cliClient struct {
	bin string
	dir string

	run      func(ctx context.Context, args ...string) ([]byte, error)
	lookPath func(string) (string, error)
}

// NewCLI returns a GraphClient backed by the Entire Graph command line.
//
// It shells out to:
//
//	<bin> graph search        --json <query>
//	<bin> graph impact        --json <symbol>
//	<bin> graph semantic-diff --json <fromRef> <toRef>
//
// and expects JSON on stdout: a bare array or {"symbols": [...]} for search, a
// GraphImpact object for impact, and a bare array or {"changes": [...]} for
// semantic-diff. Anything it cannot read is reported as model.ErrUnavailable
// rather than as an empty result, because "unreadable" and "nothing found" are
// different facts and only one of them is safe to act on (plan §48).
func NewCLI(bin, dir string) model.GraphClient {
	c := &cliClient{bin: bin, dir: dir, lookPath: exec.LookPath}
	c.run = c.shell
	return c
}

// Available reports whether the configured binary resolves on PATH. It does not
// execute anything: probing must never be the reason a command runs.
func (c *cliClient) Available(ctx context.Context) bool {
	if ctx.Err() != nil || c.bin == "" || c.lookPath == nil {
		return false
	}
	_, err := c.lookPath(c.bin)
	return err == nil
}

// Describe names the backend for output, so the reader knows which Graph
// produced a verification.
func (c *cliClient) Describe() string {
	return fmt.Sprintf("entire graph CLI (%s) over %s", c.bin, dirLabel(c.dir))
}

// Search resolves a target to concrete definitions. An empty result with a nil
// error means Graph ran and found nothing, which is a usable fact.
func (c *cliClient) Search(ctx context.Context, query string) ([]model.GraphSymbol, error) {
	out, err := c.run(ctx, "graph", "search", "--json", query)
	if err != nil {
		return nil, err
	}
	syms, err := decodeSymbols(out)
	if err != nil {
		return nil, err
	}
	sortSymbols(syms)
	return syms, nil
}

// Impact returns the blast radius the CLI reports for a symbol.
func (c *cliClient) Impact(ctx context.Context, symbol string) (model.GraphImpact, error) {
	out, err := c.run(ctx, "graph", "impact", "--json", symbol)
	if err != nil {
		return model.GraphImpact{}, err
	}
	imp, err := decodeImpact(out)
	if err != nil {
		return model.GraphImpact{}, err
	}
	sortImpact(&imp)
	return imp, nil
}

// SemanticDiff compares two revisions through the CLI (plan §36, moment 3).
func (c *cliClient) SemanticDiff(ctx context.Context, fromRef, toRef string) ([]model.SemanticChange, error) {
	out, err := c.run(ctx, "graph", "semantic-diff", "--json", fromRef, toRef)
	if err != nil {
		return nil, err
	}
	changes, err := decodeChanges(out)
	if err != nil {
		return nil, err
	}
	sortChanges(changes)
	return changes, nil
}

// shell runs the Entire binary and returns its stdout.
//
// stderr is deliberately dropped rather than folded into the error: command
// output can carry environment detail and must not be copied into state or
// rendered output (plan §34).
func (c *cliClient) shell(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	if c.dir != "" {
		cmd.Dir = c.dir
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s %s failed: %v", model.ErrUnavailable, c.bin, strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

// decodeSymbols reads a search payload. Empty output is treated as "no
// matches"; malformed output is treated as unavailable, never as no matches.
func decodeSymbols(raw []byte) ([]model.GraphSymbol, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return []model.GraphSymbol{}, nil
	}
	if trimmed[0] == '[' {
		var out []model.GraphSymbol
		if err := json.Unmarshal(trimmed, &out); err != nil {
			return nil, unreadable("search", err)
		}
		return out, nil
	}
	var wrapper struct {
		Symbols []model.GraphSymbol `json:"symbols"`
	}
	if err := json.Unmarshal(trimmed, &wrapper); err != nil {
		return nil, unreadable("search", err)
	}
	if wrapper.Symbols == nil {
		wrapper.Symbols = []model.GraphSymbol{}
	}
	return wrapper.Symbols, nil
}

// decodeImpact reads an impact payload. Unlike search, an empty body is not a
// usable answer: a real impact response always names its target, so silence
// here would be indistinguishable from "nothing depends on this symbol".
func decodeImpact(raw []byte) (model.GraphImpact, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return model.GraphImpact{}, fmt.Errorf("%w: the Graph CLI returned no impact payload", model.ErrUnavailable)
	}
	var imp model.GraphImpact
	if err := json.Unmarshal(trimmed, &imp); err != nil {
		return model.GraphImpact{}, unreadable("impact", err)
	}
	return imp, nil
}

// decodeChanges reads a semantic-diff payload. Empty output means "no semantic
// change between the revisions", which is a legitimate answer.
func decodeChanges(raw []byte) ([]model.SemanticChange, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return []model.SemanticChange{}, nil
	}
	if trimmed[0] == '[' {
		var out []model.SemanticChange
		if err := json.Unmarshal(trimmed, &out); err != nil {
			return nil, unreadable("semantic-diff", err)
		}
		return out, nil
	}
	var wrapper struct {
		Changes []model.SemanticChange `json:"changes"`
	}
	if err := json.Unmarshal(trimmed, &wrapper); err != nil {
		return nil, unreadable("semantic-diff", err)
	}
	if wrapper.Changes == nil {
		wrapper.Changes = []model.SemanticChange{}
	}
	return wrapper.Changes, nil
}

// unreadable reports a payload this build cannot parse. The parser error is
// included because it names the offset, not the content.
func unreadable(command string, err error) error {
	return fmt.Errorf("%w: the Graph CLI returned %s output this build cannot read: %v", model.ErrUnavailable, command, err)
}
