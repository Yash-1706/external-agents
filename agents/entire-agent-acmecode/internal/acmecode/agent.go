// Package acmecode is the Entire external agent for AcmeCode.
//
// What makes this integration different from its siblings is the transcript
// layer. AcmeCode shipped a new lifecycle JSONL format while its existing
// installs kept emitting the old one, so a build that understands only one of
// them is wrong for half the fleet from the day it lands. The reader here is
// therefore format-aware rather than format-assuming: it detects the shape of
// each record, decodes both through one engine, retains records whose lifecycle
// name it does not recognise instead of dropping them, and turns a truncated
// transcript into a partial result rather than a discarded session.
//
// The engine doing that work lives in internal/continuity and is shared with
// the task-continuity layer; this package is the protocol surface over it.
package acmecode

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/normalize"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/protocol"
)

// sessionDirName is where AcmeCode writes its session transcripts, relative to
// the repository root.
const sessionDirName = ".acmecode/sessions"

// Agent implements the external-agent protocol for AcmeCode.
type Agent struct{}

// New returns an AcmeCode agent.
func New() *Agent { return &Agent{} }

// Info declares what this integration can do.
//
// Hooks are deliberately not declared. AcmeCode exposes no documented hook
// installation surface that this build has been able to verify, and declaring a
// capability we cannot honour would make Entire install hooks that never fire —
// which reads to a user as "capture is working" while nothing is captured.
// Transcript analysis is what we can actually do, so it is what we claim.
func (a *Agent) Info() protocol.InfoResponse {
	return protocol.InfoResponse{
		ProtocolVersion: protocol.ProtocolVersion,
		Name:            "acmecode",
		Type:            "AcmeCode",
		Description:     "AcmeCode - External agent plugin for Entire CLI, reading both the original and the v2 lifecycle transcript formats",
		IsPreview:       true,
		ProtectedDirs:   []string{".acmecode"},
		HookNames:       []string{},
		Capabilities: protocol.DeclaredCapabilities{
			TranscriptAnalyzer: true,
		},
	}
}

// Detect reports whether AcmeCode is in use in this repository.
func (a *Agent) Detect() protocol.DetectResponse {
	_, err := os.Stat(filepath.Join(protocol.RepoRoot(), ".acmecode"))
	return protocol.DetectResponse{Present: err == nil}
}

// GetSessionDir returns the directory holding AcmeCode session transcripts.
func (a *Agent) GetSessionDir(repoPath string) (string, error) {
	if strings.TrimSpace(repoPath) == "" {
		repoPath = protocol.RepoRoot()
	}
	return filepath.Join(repoPath, filepath.FromSlash(sessionDirName)), nil
}

// ResolveSessionFile maps a session id onto its transcript file.
func (a *Agent) ResolveSessionFile(sessionDir, sessionID string) string {
	return filepath.Join(sessionDir, sessionID+".jsonl")
}

// GetSessionID reads the session id out of a hook payload, falling back to the
// session ref. An empty result is honest: it means the payload did not name one.
func (a *Agent) GetSessionID(input *protocol.HookInputJSON) string {
	if input == nil {
		return ""
	}
	if id := strings.TrimSpace(input.SessionID); id != "" {
		return id
	}
	return strings.TrimSpace(input.SessionRef)
}

// ReadTranscript returns the raw transcript bytes for a session.
//
// A missing transcript is not an error. A session whose file has not been
// written yet is an ordinary state, and reporting it as a failure would make
// Entire treat a young session as a broken one.
func (a *Agent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(resolvePath(sessionRef))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

// FormatResumeCommand returns the command a user runs to resume a session.
func (a *Agent) FormatResumeCommand(sessionID string) string {
	return "acmecode --resume " + sessionID
}

// GetTranscriptPosition returns how far into the transcript we have already
// read, measured in records rather than bytes so that a rewritten line cannot
// silently shift the offset.
func (a *Agent) GetTranscriptPosition(path string) (int, error) {
	res, err := a.decode(path)
	if err != nil {
		return 0, err
	}
	return res.Report.Total, nil
}

// ExtractModifiedFiles returns the files this session touched, along with the
// new read position.
//
// Only records after offset are considered, so an incremental read does not
// re-report work Entire has already seen.
func (a *Agent) ExtractModifiedFiles(path string, offset int) ([]string, int, error) {
	res, err := a.decode(path)
	if err != nil {
		return nil, 0, err
	}

	seen := map[string]bool{}
	var files []string
	for i, ev := range res.Events {
		if i < offset {
			continue
		}
		p := strings.TrimSpace(ev.Attr("path"))
		// A read is not a modification. Reporting one would inflate the change
		// set with files the agent only looked at.
		if p == "" || ev.Attr("tool_name") != "file_changed" || seen[p] {
			continue
		}
		seen[p] = true
		files = append(files, p)
	}
	return files, res.Report.Total, nil
}

// ExtractPrompts returns the user prompts in the session, in order.
func (a *Agent) ExtractPrompts(sessionRef string, offset int) ([]string, error) {
	res, err := a.decode(resolvePath(sessionRef))
	if err != nil {
		return nil, err
	}

	var prompts []string
	for i, ev := range res.Events {
		if i < offset || ev.Type != model.TurnStarted {
			continue
		}
		if s := strings.TrimSpace(ev.Summary); s != "" {
			prompts = append(prompts, s)
		}
	}
	return prompts, nil
}

// ExtractSummary returns a one-line account of what the session did.
//
// A checkpoint's stated intent wins over the agent's closing message: it is the
// considered statement of the goal rather than the last thing said. When the
// transcript was only partly readable the summary says so, because a partial
// reconstruction presented as a complete one is the failure this whole
// integration is built to avoid.
func (a *Agent) ExtractSummary(sessionRef string) (string, bool, error) {
	res, err := a.decode(resolvePath(sessionRef))
	if err != nil {
		return "", false, err
	}
	if len(res.Events) == 0 {
		return "", false, nil
	}

	summary := ""
	for _, ev := range res.Events {
		if ev.Type == model.CheckpointCreated {
			if intent := strings.TrimSpace(ev.Attr("intent")); intent != "" {
				summary = intent
				break
			}
		}
	}
	if summary == "" {
		for i := len(res.Events) - 1; i >= 0; i-- {
			if res.Events[i].Type == model.TurnEnded {
				if s := strings.TrimSpace(res.Events[i].Summary); s != "" {
					summary = s
					break
				}
			}
		}
	}
	if summary == "" {
		return "", false, nil
	}

	if !res.Report.Complete() {
		summary += " [transcript read incompletely: " + res.Report.Gaps() + "]"
	}
	return summary, true, nil
}

// decode reads and normalizes a transcript.
//
// Every entry point funnels through here so that format detection, unknown-record
// retention and truncation tolerance apply uniformly. A transcript that cannot
// be read at all yields no events and no error: Entire asks about sessions that
// may not have started yet, and an error there would be noise, not information.
func (a *Agent) decode(path string) (normalize.Result, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return normalize.Result{}, nil
	}
	if err != nil {
		return normalize.Result{}, err
	}
	res, err := normalize.Stream(data, normalize.Options{TaskID: taskIDFor(path)})
	if err != nil {
		// The stream was not JSON in any framing. That is the wrong file, not a
		// partial one, and there is nothing honest to salvage from it.
		return normalize.Result{}, nil
	}
	return res, nil
}

// resolvePath accepts either a session ref or a direct path.
func resolvePath(sessionRef string) string {
	if strings.HasSuffix(sessionRef, ".jsonl") || filepath.IsAbs(sessionRef) {
		return sessionRef
	}
	return filepath.Join(protocol.RepoRoot(), filepath.FromSlash(sessionDirName), sessionRef+".jsonl")
}

// taskIDFor anchors decoded events to a task derived from the transcript's own
// name. The continuity engine requires every event to name a task; for protocol
// calls the identity only has to be stable within one transcript.
func taskIDFor(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if base == "" || base == "." {
		return "acmecode_session"
	}
	return "acmecode_" + base
}
