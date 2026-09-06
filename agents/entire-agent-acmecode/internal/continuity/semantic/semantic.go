// Package semantic implements the model-assisted half of task state (plan §30):
// requirements, decisions, rejected approaches, assumptions and next actions.
//
// No LLM is reachable in this build, so the shipped Extractor is deterministic
// and rule-based. That is not an apology for a missing model. Plan Rule 6
// forbids an LLM from ever being the canonical source of truth, so the
// model-assisted half is deliberately confined behind model.Extractor: it may
// only propose claims, every claim it proposes must cite an artifact, and the
// whole result is merged underneath Level 1 repository facts (plan §31).
// Substituting a model-backed Extractor later must not change any of those
// properties — which is exactly why they live in the interface and not in the
// implementation.
//
// Two invariants in here are correctness requirements, not style:
//
//   - No requirement is ever emitted as model.ReqComplete. Keyword overlap is
//     not proof that an obligation was met, and plan §32 forbids marking a
//     requirement complete without sufficient evidence.
//   - No claim is emitted without a citation that points at something. A claim
//     nobody can check is worse than an absent claim, so unevidenced candidates
//     are dropped. "Has evidence" is not a length test: an Evidence carrying a
//     kind but an empty ref names no artifact, yet model.ConfidenceFor still
//     reads its kind and would rate the claim Observed. Records like that are
//     treated as absent, and confidence is re-derived from what survives.
package semantic

// cue is a lexical trigger that promotes a piece of text into a claim.
//
// Cue vocabularies are fixed, ordered slices rather than maps: the first cue
// that matches is recorded on the claim's evidence as the reason the text was
// selected, so the vocabulary order is part of the output and must never depend
// on map iteration (determinism is a product requirement, plan §33).
type cue string

// Vocabularies. These are the entire "understanding" the heuristic has; keeping
// them in one block makes the extractor's competence auditable at a glance,
// which matters more here than cleverness would.
var (
	// requirementCues mark an obligation stated in prose. Modal verbs plus the
	// negative form "without" cover how tasks are actually phrased to agents.
	requirementCues = []cue{"must", "should", "shall", "needs to", "without", "ensure", "prevent", "add", "implement"}

	// decisionCues mark a durable engineering choice (plan §14).
	decisionCues = []cue{"decided", "chose", "chosen", "instead of", "switched to", "went with"}

	// rejectionCues mark an approach tried and abandoned, retained so a later
	// worker does not rediscover the same failure (plan §15, §32).
	rejectionCues = []cue{"reverted", "rejected", "does not work", "broke", "abandoned", "failed because", "backed out"}

	// assumptionCues mark something the task relies on that is not proven.
	assumptionCues = []cue{"assume", "assuming", "presumably"}
)

// Evidence refs used for the two non-file sources this extractor reads. They are
// constants because the drift and merge layers match on them.
const (
	sourceOriginalPrompt = "original_prompt"
	refTranscript        = "transcript"
)
