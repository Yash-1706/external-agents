package render

import (
	"time"

	"github.com/entireio/external-agents/agents/entire-agent-acmecode/internal/continuity/model"
)

// base is the fixed origin every fixture timestamp is derived from. Fixtures
// never read a clock: a golden file that moved when the wall clock moved would
// be proving nothing (model.Clock exists for exactly this reason).
var base = time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

func commitEv(sha string) model.Evidence {
	return model.Evidence{Kind: model.EvidenceCommit, Ref: sha, CapturedAt: at(10)}
}

// fileEv builds a file+lines citation. Path carries the location and Ref is
// left empty on purpose: model.Evidence.String prints Ref and Path one after
// the other, so setting both would render the path twice.
func fileEv(path string, start, end int) model.Evidence {
	return model.Evidence{Kind: model.EvidenceFile, Path: path, LineStart: start, LineEnd: end}
}

func testEv(name, result string) model.Evidence {
	return model.Evidence{Kind: model.EvidenceTest, Ref: name, Result: result}
}

func cpEv(id string) model.Evidence {
	return model.Evidence{Kind: model.EvidenceCheckpoint, Ref: id, SessionID: "ses-oc-1"}
}

func promptEv(detail string) model.Evidence {
	return model.Evidence{Kind: model.EvidencePrompt, Ref: "ses-oc-1", Detail: detail}
}

func inferenceEv(detail string) model.Evidence {
	return model.Evidence{Kind: model.EvidenceInference, Ref: "extractor", Detail: detail}
}

// fullState is a task with complete capture: every claim has evidence, the
// lineage is intact, and no honesty caveats should fire.
func fullState() *model.EngineeringState {
	s := model.NewEngineeringState(model.TaskRef{
		ID:             "TASK-4417",
		Title:          "Implement subscription pause",
		OriginalIntent: "Pause future billing while preserving the current billing cycle, and leave legacy renewal behaviour untouched.",
	})
	s.Status = model.StatusPartial
	s.Repo = model.RepoState{
		Repo:       "acme/billing",
		Branch:     "feat/pause",
		CommitSHA:  "a1b2c3d4e5f6",
		Dirty:      true,
		TreeHash:   "tree-9f2a",
		ObservedAt: at(120),
	}
	s.Capture = model.Capture{
		GitAvailable:        true,
		CheckpointAvailable: true,
		TranscriptAvailable: true,
		EventsAvailable:     true,
		GraphAvailable:      true,
		TestResultsParsed:   true,
		SemanticExtraction:  true,
	}
	s.Requirements = []model.Requirement{
		{ID: "R1", Description: "Expose a pause endpoint on the subscription API.", Status: model.ReqComplete, Confidence: model.Observed, Source: "original prompt", Evidence: []model.Evidence{commitEv("a1b2c3d4e5f6"), testEv("TestPauseEndpoint", "passed")}, UpdatedAt: at(40)},
		{ID: "R2", Description: "Authorize pause requests against the billing owner.", Status: model.ReqComplete, Confidence: model.Observed, Source: "original prompt", Evidence: []model.Evidence{fileEv("billing/authz.go", 44, 71)}, UpdatedAt: at(55)},
		{ID: "R3", Description: "Make webhook handling idempotent under replay.", Status: model.ReqPartial, Confidence: model.Inferred, Source: "original prompt", Evidence: []model.Evidence{testEv("TestDuplicateWebhook", "failed"), inferenceEv("partial implementation observed in diff")}, UpdatedAt: at(95)},
		{ID: "R4", Description: "Add regression tests for the paused billing cycle.", Status: model.ReqUnresolved, Confidence: model.Unknown, Source: "original prompt", Evidence: nil, UpdatedAt: at(95)},
		{ID: "R5", Description: "Keep legacy renewal behaviour unchanged.", Status: model.ReqBlocked, Confidence: model.Observed, Source: "discovered constraint", Evidence: []model.Evidence{testEv("TestLegacyRenewal", "failed")}, UpdatedAt: at(100)},
	}
	s.CompletedWork = []model.WorkItem{
		{Description: "Added a pause state column and its migration.", Confidence: model.Observed, Evidence: []model.Evidence{commitEv("9f8e7d6c5b4a")}, SessionID: "ses-oc-1"},
		{Description: "Wired the pause endpoint into the billing router.", Confidence: model.Observed, Evidence: []model.Evidence{fileEv("billing/router.go", 12, 18)}, SessionID: "ses-oc-1"},
	}
	s.InProgress = []model.WorkItem{
		{Description: "Deduplicating webhook deliveries by event id.", Confidence: model.Inferred, Evidence: []model.Evidence{fileEv("billing/webhooks.go", 88, 140)}, SessionID: "ses-hm-1"},
	}
	s.FailedAttempts = []model.FailedAttempt{
		{Description: "Deduplicating on payload hash collided for retried events.", Evidence: []model.Evidence{testEv("TestDuplicateWebhook", "failed")}, SessionID: "ses-oc-1", OccurredAt: at(70)},
	}
	s.Decisions = []model.Decision{
		{ID: "D1", Decision: "Model pause as an explicit subscription state.", Reason: "An explicit state keeps renewal arithmetic in one place and is inspectable in support tooling.", Evidence: []model.Evidence{cpEv("cp-a"), commitEv("9f8e7d6c5b4a")}, SessionID: "ses-oc-1", CheckpointID: "cp-a", MadeAt: at(25)},
		{ID: "D0", Decision: "Derive pause from a null renewal date.", Reason: "Smallest possible diff.", Evidence: []model.Evidence{promptEv("early plan in session ses-oc-1")}, SessionID: "ses-oc-1", MadeAt: at(15), SupersededBy: "D1"},
	}
	s.Rejected = []model.RejectedApproach{
		{ID: "RA1", Approach: "Mutate the renewal status directly on pause.", Reason: "It breaks legacy renewal behaviour; TestLegacyRenewal fails.", Confidence: model.Observed, Evidence: []model.Evidence{testEv("TestLegacyRenewal", "failed")}, SessionID: "ses-oc-1", CheckpointID: "cp-a", RejectedAt: at(30)},
	}
	s.Assumptions = []model.Assumption{
		{Statement: "Webhook deliveries carry a stable provider event id.", Confidence: model.Inferred, Evidence: []model.Evidence{fileEv("billing/webhooks.go", 30, 36)}},
		{Statement: "No customer is mid-proration when a pause lands.", Confidence: model.Unknown, Evidence: nil, Invalidated: true},
	}
	s.Tests = []model.TestResult{
		{Name: "TestPauseEndpoint", Status: model.TestPassed, Command: "go test ./billing/...", Package: "billing", RanAt: at(110), Evidence: []model.Evidence{testEv("TestPauseEndpoint", "passed")}},
		{Name: "TestDuplicateWebhook", Status: model.TestFailed, Command: "go test ./billing/...", Package: "billing", RanAt: at(110), Files: []string{"billing/webhooks.go", "billing/webhooks_test.go"}, Evidence: []model.Evidence{testEv("TestDuplicateWebhook", "failed")}},
		{Name: "TestLegacyRenewal", Status: model.TestFailed, Command: "go test ./billing/...", Package: "billing", RanAt: at(110), Evidence: []model.Evidence{testEv("TestLegacyRenewal", "failed")}},
	}
	s.ChangedFiles = []model.ChangedFile{
		{Path: "billing/subscription.go", Status: "M", Insert: 62, Delete: 9, Evidence: []model.Evidence{commitEv("a1b2c3d4e5f6")}},
		{Path: "billing/webhooks.go", Status: "M", Insert: 31, Delete: 4},
		{Path: "billing/pause_test.go", Status: "A", Insert: 88, Delete: 0},
	}
	s.Risks = []model.Risk{
		{Description: "A replayed webhook can double-apply a pause until idempotency lands.", Severity: "high", Confidence: model.Observed, Evidence: []model.Evidence{testEv("TestDuplicateWebhook", "failed")}},
	}
	s.NextActions = []model.NextAction{
		{Description: "Make the webhook handler idempotent on provider event id, then rerun the billing suite.", Rationale: "TestDuplicateWebhook is the only failure blocking R3.", Target: "billing.HandleWebhook", Priority: 1, Confidence: model.Recommended, GraphVerified: true, SuggestedTests: []string{"TestDuplicateWebhook", "TestPauseEndpoint"}, Evidence: []model.Evidence{testEv("TestDuplicateWebhook", "failed"), model.Evidence{Kind: model.EvidenceGraph, Ref: "billing.HandleWebhook", Detail: "3 callers, 2 tests"}}},
		{Description: "Restore legacy renewal behaviour before merging.", Rationale: "TestLegacyRenewal regressed and R5 is blocked on it.", Target: "billing.Renew", Priority: 2, Confidence: model.Recommended, Evidence: []model.Evidence{testEv("TestLegacyRenewal", "failed")}},
	}
	s.Evidence = []model.Evidence{cpEv("cp-a"), cpEv("cp-b")}
	s.GeneratedAt = at(120)
	return s
}

// fullLineage carries the task across three runtimes, which is the observable
// proof of cross-agent continuity the product exists to produce (plan §44).
// It also contains two deliberately imperfect checkpoints so the tree is
// exercised on lineage that is not clean.
func fullLineage() model.Lineage {
	return model.Lineage{
		Task: model.Task{
			ID:            "TASK-4417",
			Repo:          "acme/billing",
			Branch:        "feat/pause",
			RootSessionID: "ses-oc-1",
			Title:         "Implement subscription pause",
			Status:        model.StatusPartial,
			CreatedAt:     base,
			UpdatedAt:     at(120),
		},
		Sessions: []model.SessionNode{
			{SessionID: "ses-oc-1", Agent: model.AgentOpenClaw, Role: model.RoleMain, StartedAt: base, EndedAt: at(45)},
			{SessionID: "ses-oc-2", ParentSessionID: "ses-oc-1", Agent: model.AgentOpenClaw, Role: model.RoleTest, StartedAt: at(20), Interrupted: true},
			{SessionID: "ses-hm-1", ParentSessionID: "ses-oc-1", Agent: model.AgentHermes, Role: model.RoleCoding, StartedAt: at(50), EndedAt: at(105)},
			{SessionID: "ses-hu-1", Agent: model.AgentHuman, Role: model.RoleHuman, StartedAt: at(110)},
		},
		Checkpoints: []model.CheckpointNode{
			{CheckpointID: "cp-a", SessionID: "ses-oc-1", Agent: model.AgentOpenClaw, Label: "pause state chosen", CommitSHA: "9f8e7d6c5b4a", Branch: "feat/pause", CreatedAt: at(25), StateHash: "sha256:1111"},
			{CheckpointID: "cp-b", SessionID: "ses-hm-1", Agent: model.AgentHermes, Label: "webhook dedupe wip", CommitSHA: "a1b2c3d4e5f6", Branch: "feat/pause", CreatedAt: at(100), StateHash: "sha256:2222"},
			{CheckpointID: "cp-c", Agent: model.AgentHermes, Label: "adapter lost the session id", CreatedAt: at(102)},
			{CheckpointID: "cp-d", SessionID: "ses-gone", Agent: model.AgentUnknown, CreatedAt: at(104)},
		},
		Handoffs: []model.HandoffNode{
			{ID: "ho-1", TaskID: "TASK-4417", FromSessionID: "ses-oc-1", FromAgent: model.AgentOpenClaw, ToSessionID: "ses-hm-1", ToAgent: model.AgentHermes, SourceCheckpoint: "cp-a", StateHash: "sha256:1111", CreatedAt: at(48), NextAction: "Finish webhook idempotency.", Outstanding: 3, CriticalFailures: 1},
			{ID: "ho-2", TaskID: "TASK-4417", FromSessionID: "ses-hm-1", FromAgent: model.AgentHermes, ToSessionID: "ses-hu-1", ToAgent: model.AgentHuman, SourceCheckpoint: "cp-b", StateHash: "sha256:2222", CreatedAt: at(108), NextAction: "Review the pause state model.", Outstanding: 2, CriticalFailures: 2},
		},
	}
}

// partialState simulates the plan §47 acceptance test: an external agent that
// did not expose everything. Nothing here may be presented as complete.
func partialState() *model.EngineeringState {
	s := model.NewEngineeringState(model.TaskRef{
		ID:    "TASK-4417",
		Title: "Implement subscription pause",
	})
	s.Status = model.StatusResumed
	s.Repo = model.RepoState{Repo: "acme/billing", Branch: "feat/pause", CommitSHA: "a1b2c3d4e5f6", ObservedAt: at(120)}
	s.Capture = model.Capture{
		GitAvailable:        true,
		CheckpointAvailable: false,
		TranscriptAvailable: false,
		EventsAvailable:     true,
		GraphAvailable:      false,
		TestResultsParsed:   false,
		SemanticExtraction:  false,
		Missing: []string{
			"original decision rationale",
			"agent transcript for session ses-oc-2",
			"entire checkpoint history",
		},
		Notes: []string{
			"The OpenClaw adapter did not expose turn-level events for ses-oc-2.",
			"Entire was unreachable during capture; checkpoint ids could not be resolved.",
		},
	}
	s.Requirements = []model.Requirement{
		{ID: "R1", Description: "Expose a pause endpoint on the subscription API.", Status: model.ReqComplete, Confidence: model.Observed, Evidence: []model.Evidence{commitEv("a1b2c3d4e5f6")}},
		{ID: "R2", Description: "Make webhook handling idempotent under replay.", Status: model.ReqUnknown, Confidence: model.Unknown},
	}
	s.ChangedFiles = []model.ChangedFile{
		{Path: "billing/subscription.go", Status: "M", Insert: 62, Delete: 9},
		{Path: "billing/webhooks.go", Status: "?", Insert: 0, Delete: 0},
	}
	s.Evidence = []model.Evidence{commitEv("a1b2c3d4e5f6")}
	s.GeneratedAt = at(120)
	return s
}

// partialLineage has no checkpoints at all, so a resume cannot be verified
// against one (plan §48).
func partialLineage() model.Lineage {
	return model.Lineage{
		Task: model.Task{ID: "TASK-4417", Repo: "acme/billing", Branch: "feat/pause", Title: "Implement subscription pause"},
		Sessions: []model.SessionNode{
			{SessionID: "ses-oc-2", Agent: model.AgentOpenClaw, Role: model.RoleTest, StartedAt: at(20), Interrupted: true},
		},
	}
}

// emptyState is a state with nothing in it. Every view must still say what it
// does not know rather than rendering a reassuring blank.
func emptyState() *model.EngineeringState {
	return model.NewEngineeringState(model.TaskRef{})
}

// constraintState exercises the §41 curveball: a requirement arriving mid-task
// that annotates the original intent instead of replacing it.
func constraintState() *model.EngineeringState {
	s := model.NewEngineeringState(model.TaskRef{
		ID:             "TASK-4417",
		Title:          "Implement subscription pause",
		OriginalIntent: "Pause future billing while preserving the current billing cycle.",
	})
	s.Status = model.StatusActive
	s.Repo = model.RepoState{Repo: "acme/billing", Branch: "feat/pause", CommitSHA: "a1b2c3d4e5f6", ObservedAt: at(200)}
	s.Capture = model.Capture{
		GitAvailable:        true,
		CheckpointAvailable: true,
		TranscriptAvailable: true,
		EventsAvailable:     true,
		GraphAvailable:      true,
		TestResultsParsed:   true,
		SemanticExtraction:  true,
	}
	s.Requirements = []model.Requirement{
		{ID: "R1", Description: "Expose a pause endpoint on the subscription API.", Status: model.ReqComplete, Confidence: model.Observed, Source: "original prompt", Evidence: []model.Evidence{commitEv("a1b2c3d4e5f6")}},
		{ID: "R2", Description: "Pause takes effect at the end of the current billing cycle.", Status: model.ReqPartial, Confidence: model.Inferred, Source: "original prompt", SupersededBy: "C1", Evidence: []model.Evidence{fileEv("billing/subscription.go", 210, 244)}},
		{ID: "R3", Description: "A paused subscription must be resumable within 30 days.", Status: model.ReqUnresolved, Confidence: model.Unknown, Source: "constraint C1"},
	}
	s.Constraints = []model.Constraint{
		{
			ID:                   "C1",
			Text:                 "Paused subscriptions must auto-resume after 30 days unless cancelled.",
			Source:               "noon curveball",
			AddedAt:              at(180),
			AffectedRequirements: []string{"R2", "R3"},
			ChangedAssumptions:   []string{"A pause lasts until the customer resumes it."},
			Evidence:             []model.Evidence{model.Evidence{Kind: model.EvidenceEvent, Ref: "ev-constraint-1", Detail: "ConstraintAdded"}},
		},
	}
	s.Assumptions = []model.Assumption{
		{Statement: "A pause lasts until the customer resumes it.", Confidence: model.Inferred, Invalidated: true, Evidence: []model.Evidence{promptEv("original prompt")}},
	}
	s.Tests = []model.TestResult{
		{Name: "TestPauseEndpoint", Status: model.TestPassed, Command: "go test ./billing/...", Package: "billing", RanAt: at(190), Evidence: []model.Evidence{testEv("TestPauseEndpoint", "passed")}},
	}
	s.ChangedFiles = []model.ChangedFile{
		{Path: "billing/subscription.go", Status: "M", Insert: 62, Delete: 9},
	}
	s.NextActions = []model.NextAction{
		{Description: "Add the 30-day auto-resume job and a test that proves it fires.", Rationale: "C1 arrived after the original plan and nothing implements it yet.", Target: "billing.AutoResume", Priority: 1, Confidence: model.Recommended, Evidence: []model.Evidence{model.Evidence{Kind: model.EvidenceEvent, Ref: "ev-constraint-1", Detail: "ConstraintAdded"}}},
	}
	s.GeneratedAt = at(200)
	return s
}

func fullReceipt() model.HandoffNode {
	return model.HandoffNode{
		ID:               "ho-2",
		TaskID:           "TASK-4417",
		FromSessionID:    "ses-hm-1",
		FromAgent:        model.AgentHermes,
		ToSessionID:      "ses-hu-1",
		ToAgent:          model.AgentHuman,
		SourceCheckpoint: "cp-b",
		StateHash:        "sha256:2222",
		CreatedAt:        at(108),
		NextAction:       "Make the webhook handler idempotent on provider event id.",
		Outstanding:      2,
		CriticalFailures: 1,
	}
}

// bareReceipt is a handoff that was recorded without the fields that make it
// checkable. The receipt must admit that rather than render blanks.
func bareReceipt() model.HandoffNode {
	return model.HandoffNode{TaskID: "TASK-4417", Outstanding: 1}
}

// overstatedState is a state whose extractor rated every claim OBSERVED
// without recording a single citation. Nothing here may render as a fact.
func overstatedState() *model.EngineeringState {
	s := model.NewEngineeringState(model.TaskRef{
		ID:             "TASK-4417",
		Title:          "Implement subscription pause",
		OriginalIntent: "Pause future billing while preserving the current billing cycle.",
	})
	s.Status = model.StatusVerified
	s.Capture = model.Capture{
		GitAvailable:        true,
		CheckpointAvailable: true,
		TranscriptAvailable: true,
		EventsAvailable:     true,
		GraphAvailable:      true,
		TestResultsParsed:   true,
		SemanticExtraction:  true,
	}
	s.Requirements = []model.Requirement{
		{ID: "R1", Description: "Expose a pause endpoint on the subscription API.", Status: model.ReqComplete, Confidence: model.Observed},
	}
	s.CompletedWork = []model.WorkItem{{Description: "Everything the original prompt asked for was implemented and verified.", Confidence: model.Observed}}
	s.Assumptions = []model.Assumption{{Statement: "Webhook deliveries carry a stable provider event id.", Confidence: model.Observed}}
	s.Risks = []model.Risk{{Description: "A replayed webhook can double-apply a pause.", Severity: "high", Confidence: model.Observed}}
	s.Rejected = []model.RejectedApproach{{ID: "RA1", Approach: "Mutate the renewal status directly on pause.", Reason: "It breaks legacy renewal behaviour.", Confidence: model.Observed}}
	return s
}

// degradedFullState is fullState with the three capture inputs that
// model.Capture.Complete does not inspect turned off, plus a note. Complete
// reports true for it, so it is the state that used to render as a full
// recovery.
func degradedFullState() *model.EngineeringState {
	s := fullState()
	s.Capture.GraphAvailable = false
	s.Capture.TestResultsParsed = false
	s.Capture.SemanticExtraction = false
	s.Capture.Notes = []string{"Entire graph and semantic extraction were unreachable during capture."}
	return s
}
