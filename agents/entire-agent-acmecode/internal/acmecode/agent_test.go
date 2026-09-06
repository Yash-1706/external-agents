package acmecode

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "continuity", "normalize", "testdata", "lifecycle_v2_acmecode.jsonl"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return b
}

// TestInfoDeclaresOnlyWhatItImplements guards the rule that a capability we
// cannot honour must not be declared: Entire would install hooks that never
// fire, which reads to a user as working capture while nothing is captured.
func TestInfoDeclaresOnlyWhatItImplements(t *testing.T) {
	info := New().Info()
	if !info.Capabilities.TranscriptAnalyzer {
		t.Error("transcript_analyzer is implemented but not declared")
	}
	if info.Capabilities.Hooks {
		t.Error("hooks declared, but no hook surface is implemented")
	}
	if len(info.HookNames) != 0 {
		t.Errorf("hook names declared without the hooks capability: %v", info.HookNames)
	}
	if info.Name != "acmecode" {
		t.Errorf("Name = %q, want acmecode", info.Name)
	}
}

// TestChunkReassembleRoundTrip is the property that matters for transport:
// chunking must be lossless, and must never split a record, or the seam becomes
// unparseable — the exact truncation failure this integration exists to survive.
func TestChunkReassembleRoundTrip(t *testing.T) {
	a := New()
	content := fixture(t)

	for _, max := range []int{0, 1, 64, 512, 4096, len(content) * 2} {
		chunks, err := a.ChunkTranscript(content, max)
		if err != nil {
			t.Fatalf("ChunkTranscript(max=%d): %v", max, err)
		}
		got, err := a.ReassembleTranscript(chunks)
		if err != nil {
			t.Fatalf("ReassembleTranscript(max=%d): %v", max, err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("round trip at max=%d lost data: %d bytes in, %d out", max, len(content), len(got))
		}
		// Every chunk must end on a record boundary.
		for i, c := range chunks {
			if len(c) > 0 && c[len(c)-1] != '\n' && i != len(chunks)-1 {
				t.Errorf("chunk %d (max=%d) does not end on a record boundary", i, max)
			}
		}
	}

	if chunks, _ := a.ChunkTranscript(nil, 100); chunks != nil {
		t.Errorf("empty content produced %d chunk(s)", len(chunks))
	}
}

// TestExtractPromptsAndSummary covers the transcript_analyzer surface against
// the real fixture: the prompt is the intent a later worker cannot reconstruct.
func TestExtractPromptsAndSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(path, fixture(t), 0o644); err != nil {
		t.Fatalf("seeding transcript: %v", err)
	}
	a := New()

	prompts, err := a.ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1", len(prompts))
	}

	summary, ok, err := a.ExtractSummary(path)
	if err != nil || !ok {
		t.Fatalf("ExtractSummary = %q, %v, %v", summary, ok, err)
	}
	// The checkpoint's stated intent wins over the closing message.
	if summary == "" {
		t.Error("empty summary for a complete transcript")
	}

	files, pos, err := a.ExtractModifiedFiles(path, 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFiles: %v", err)
	}
	if pos == 0 {
		t.Error("position did not advance")
	}
	for _, f := range files {
		// file_read must never be reported as a modification.
		if f == "src/promotions/coupon.ts" {
			t.Errorf("a file that was only read is reported as modified: %s", f)
		}
	}
}

// TestMissingTranscriptIsNotAnError: Entire asks about sessions that may not
// have started yet; an error there would make a young session look broken.
func TestMissingTranscriptIsNotAnError(t *testing.T) {
	a := New()
	path := filepath.Join(t.TempDir(), "nope.jsonl")

	if _, err := a.ReadTranscript(path); err != nil {
		t.Errorf("ReadTranscript on a missing fileErrored: %v", err)
	}
	if _, ok, err := a.ExtractSummary(path); err != nil || ok {
		t.Errorf("ExtractSummary = %v, %v; want no summary and no error", ok, err)
	}
}
