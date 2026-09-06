// Command entire-agent-acmecode is the Entire external agent for AcmeCode.
//
// It speaks the external-agent protocol: one subcommand per operation, JSON in
// on stdin and JSON out on stdout. Entire discovers it by name on PATH.
package main

import (
	"fmt"
	"os"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/acmecode"
	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/protocol"
)

func main() {
	agent := acmecode.New()

	if len(os.Args) < 2 {
		fatalf("usage: entire-agent-acmecode <subcommand> [args]")
	}

	var err error

	switch os.Args[1] {
	case "info":
		err = protocol.WriteJSON(os.Stdout, agent.Info())
	case "detect":
		err = protocol.WriteJSON(os.Stdout, agent.Detect())
	case "get-session-id":
		err = protocol.HandleGetSessionID(os.Stdin, os.Stdout, agent)
	case "get-session-dir":
		err = protocol.HandleGetSessionDir(os.Args[2:], os.Stdout, agent)
	case "resolve-session-file":
		err = protocol.HandleResolveSessionFile(os.Args[2:], os.Stdout, agent)
	case "read-session":
		err = protocol.HandleReadSession(os.Stdin, os.Stdout, agent)
	case "write-session":
		err = protocol.HandleWriteSession(os.Stdin, agent)
	case "read-transcript":
		err = protocol.HandleReadTranscript(os.Args[2:], os.Stdout, agent)
	case "chunk-transcript":
		err = protocol.HandleChunkTranscript(os.Args[2:], os.Stdin, os.Stdout, agent)
	case "reassemble-transcript":
		err = protocol.HandleReassembleTranscript(os.Stdin, os.Stdout, agent)
	case "get-transcript-position":
		err = protocol.HandleGetTranscriptPosition(os.Args[2:], os.Stdout, agent)
	case "extract-modified-files":
		err = protocol.HandleExtractModifiedFiles(os.Args[2:], os.Stdout, agent)
	case "extract-prompts":
		err = protocol.HandleExtractPrompts(os.Args[2:], os.Stdout, agent)
	case "extract-summary":
		err = protocol.HandleExtractSummary(os.Args[2:], os.Stdout, agent)
	case "format-resume-command":
		err = protocol.HandleFormatResumeCommand(os.Args[2:], os.Stdout, agent)
	default:
		// An unrecognised subcommand is a protocol mismatch worth naming, not
		// something to answer with a plausible-looking empty result.
		fatalf("unknown subcommand %q", os.Args[1])
	}

	if err != nil {
		fatalf("%s: %v", os.Args[1], err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "entire-agent-acmecode: "+format+"\n", args...)
	os.Exit(1)
}
