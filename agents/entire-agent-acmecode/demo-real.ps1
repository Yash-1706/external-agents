# Entire Continuity — the real-world workflow.
#
#   .\demo-real.ps1 -Warm     build + warm, run once offstage
#   .\demo-real.ps1           present; Enter between beats
#
# demo.ps1 shows the mechanism: one transcript in, engineering state out.
# This shows the workflow a team would actually run:
#
#   a PRD in the repo -> two teammates' agent sessions -> one task state
#   -> the system asks for the judgements it cannot make
#   -> a human answers them -> the next worker inherits everything
#
# It writes to .entire-continuity-real so it cannot disturb demo.ps1's state.

param([switch]$Warm)

$ErrorActionPreference = 'Continue'
$root    = 'D:\entire\external-agents'
$agent   = Join-Path $root 'agents\entire-agent-acmecode'
$store   = '.entire-continuity-real'
$prd     = 'examples\coupon-validation\PRD.md'
$s1      = 'internal\continuity\normalize\testdata\lifecycle_v2_acmecode.jsonl'
$s2      = 'examples\coupon-validation\session-2-priya.jsonl'

Set-Location $agent
$EC = { param($rest) & ".\entire-continuity.exe" (@('--root', $store) + $rest) }

function Beat($n, $title, $say) {
    Write-Host ''
    Write-Host ('=' * 74) -ForegroundColor DarkGray
    Write-Host "  $n  $title" -ForegroundColor Cyan
    if ($say) { Write-Host "  > $say" -ForegroundColor DarkYellow }
    Write-Host ('=' * 74) -ForegroundColor DarkGray
    Write-Host ''
}
function Cmd($text) { Write-Host "  $ $text" -ForegroundColor DarkGray }
function Next() {
    Write-Host ''
    Write-Host '  [Enter to continue]' -ForegroundColor DarkGray
    [void](Read-Host)
}

# Seed builds the whole task from scratch: the PRD, then both sessions. Used by
# -Warm and again at the start of the demo, so a second run is identical to the
# first and the human decisions from a previous run never leak into beat 1.
function Seed() {
    Remove-Item -Recurse -Force $store -ErrorAction SilentlyContinue
    & $EC @('task', 'init', '--title', 'CHK-2291 coupon validation',
            '--prd', $prd, '--session', 'prd_intake') | Out-Null
    $t = (& $EC @('task', 'list') | Select-Object -First 1).Split(' ')[0]
    & $EC @('ingest', '--task', $t, '--file', $s1, '--quiet') | Out-Null
    & $EC @('ingest', '--task', $t, '--file', $s2, '--quiet') | Out-Null
    $t
}

if ($Warm) {
    Write-Host 'Building...' -ForegroundColor Cyan
    go build -o entire-continuity.exe ./cmd/entire-continuity
    $t = Seed
    Write-Host "Ready. Task $t seeded. Run .\demo-real.ps1 to present." -ForegroundColor Green
    exit 0
}

$task = Seed
Clear-Host
Write-Host ''
Write-Host '  ENTIRE CONTINUITY — a task, across a team' -ForegroundColor White
Write-Host ''
Write-Host '  A PRD in the repository. Two engineers, each with their own agent'
Write-Host '  session. One task. The work has to survive both of them leaving.'
Next

Beat '1' 'THE SPEC LIVES IN THE REPO' 'Intent comes from a reviewed document, not from someone typing into a chatbox.'
Cmd "task init --prd $prd"
Get-Content $prd | Select-String -Pattern '^R\d' | ForEach-Object { "  $_" }
Write-Host ''
Write-Host "  task: $task" -ForegroundColor DarkGray
Next

Beat '2' 'ENGINEER ONE WORKS' 'Their agent writes a transcript. We read it.'
Cmd 'ingest --task <id> --file <arjun session>'
& $EC @('ingest', '--task', $task, '--file', $s1, '--quiet') | Select-Object -First 4
Next

Beat '3' 'ENGINEER TWO PICKS IT UP' 'A second session, a second person, the same task id.'
Cmd 'ingest --task <id> --file <priya session>'
& $EC @('ingest', '--task', $task, '--file', $s2, '--quiet') | Select-Object -First 4
Next

Beat '4' 'ONE STATE, FROM THE PRD AND BOTH SESSIONS' 'R1-R5 are the PRD''s own ids. Traceable to the ticket.'
$state = & $EC @('task', 'status', $task)
# Anchored to the whole line: the PRD's own "## Requirements" heading is quoted
# verbatim inside the INTENT block, and a loose match lands there instead.
$i = ($state | Select-String -Pattern '^REQUIREMENTS$' | Select-Object -First 1).LineNumber
$state[($i - 1)..($i + 16)]
Next

Beat '5' 'THE SYSTEM ASKS FOR WHAT IT CANNOT DECIDE' 'Not a wall of uncertainty — a queue of questions, each with why.'
Cmd 'task review'
$review = & $EC @('task', 'review', $task)
$review | Select-Object -First 12
Write-Host '  ... and the one that matters most:' -ForegroundColor DarkGray
Write-Host ''
$u = ($review | Select-String -Pattern 'UNDECIDED' | Select-Object -First 1).LineNumber
if ($u) { $review[($u - 1)..($u + 4)] }
Next

Beat '6' 'A HUMAN ANSWERS ONE' 'The judgement a machine is not entitled to make.'
Cmd 'task decide  --agent human'
& $EC @('task', 'decide', $task, '--agent', 'human', '--session', 'review_arjun',
        '--decision', 'Expiry outranks disabled in the customer-facing error',
        '--reason', 'A customer can act on an expired coupon by asking for a new one; disabled is internal state they cannot act on') |
    Select-Object -First 5
Write-Host ''
Cmd 'task reject  --agent human  --evidence apply_coupon.test.ts'
& $EC @('task', 'reject', $task, '--agent', 'human', '--session', 'review_arjun',
        '--approach', 'Return whichever validation fails first in declaration order',
        '--reason', 'Declaration order is implementation-defined, so the same coupon produced different errors between runs',
        '--evidence', 'apply_coupon.test.ts') | Select-Object -First 9
Next

Beat '7' 'THE QUEUE SHRINKS' 'The open question is answered, so it stops being asked.'
Cmd 'task review'
& $EC @('task', 'review', $task) | Select-Object -First 3
Write-Host ''
$after = & $EC @('task', 'status', $task)
$j = ($after | Select-String -Pattern '^DECISIONS' | Select-Object -First 1).LineNumber
$after[($j - 1)..($j + 11)]
Next

Beat '8' 'THE NEXT WORKER INHERITS ALL OF IT' 'Different runtime. Same task. Nothing re-derived.'
Cmd 'task resume --agent hermes  &&  task handoff --record'
& $EC @('task', 'resume', $task, '--session', 'hermes_night', '--agent', 'hermes') | Out-Null
& $EC @('task', 'handoff', $task, '--from-session', 'review_arjun', '--from-agent', 'human',
        '--to-session', 'hermes_night', '--to-agent', 'hermes', '--record') | Out-Null
& $EC @('task', 'lineage', $task)
Next

Beat '9' 'WHAT HERMES ACTUALLY RECEIVES' 'A brief ordered by what it needs first — not a transcript dump.'
$ctx = & $EC @('task', 'resume', $task, '--session', 'hermes_night', '--agent', 'hermes')
$k = ($ctx | Select-String -Pattern 'KNOWN FAILED APPROACHES' | Select-Object -First 1).LineNumber
if ($k) { $ctx[($k - 1)..($k + 9)] } else { $ctx | Select-Object -First 14 }

Write-Host ''
Write-Host ('=' * 74) -ForegroundColor DarkGray
Write-Host '  A PRD, two engineers, two agents, one human judgement.' -ForegroundColor White
Write-Host '  The task outlived every session that touched it.' -ForegroundColor White
Write-Host ('=' * 74) -ForegroundColor DarkGray
Write-Host ''
