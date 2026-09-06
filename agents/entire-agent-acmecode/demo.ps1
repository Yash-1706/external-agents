# Entire Continuity — demo driver.
#
#   .\demo.ps1 -Warm     run once before presenting: builds binaries, seeds the
#                        task, and warms the Graph index so nothing stalls live
#   .\demo.ps1           the demo; press Enter between beats
#
# Graph is reached through whichever mechanism actually launches in THIS shell.
# The managed plugin path resolves in some shells and not others, so it is
# probed rather than assumed, and a captured run is kept as a last resort — a
# demo should never die on a tooling problem.

param([switch]$Warm)

$ErrorActionPreference = 'Continue'

# The state renderer uses ✓ ⛔ ✗ • and em dashes. A console left on the
# legacy codepage renders those as mojibake, which makes a careful product
# look broken in the one place a judge is looking.
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding = [System.Text.Encoding]::UTF8

$root    = 'D:\entire\external-agents'
$agent   = Join-Path $root 'agents\entire-agent-acmecode'
$fixture = 'internal\continuity\normalize\testdata\lifecycle_v2_acmecode.jsonl'
$task    = 'task_fb86c4d96b3163f1'

Set-Location $agent

function Resolve-Graph {
    $candidates = @(
        (Join-Path $PSScriptRoot 'graph.exe'),
        (Join-Path $env:LOCALAPPDATA 'entire\plugins\bin\entire-graph.exe')
    )
    foreach ($c in $candidates) {
        if (-not (Test-Path $c)) { continue }
        $v = $null
        try { $v = & $c version 2>&1 | Select-Object -First 1 } catch { continue }
        if ($v -and "$v" -match '^v?\d') { return @{ Kind = 'direct'; Exe = $c } }
    }
    $v = $null
    try { $v = entire graph version 2>&1 | Select-Object -First 1 } catch { $v = $null }
    if ($v -and "$v" -match '^v?\d') { return @{ Kind = 'cli' } }
    return @{ Kind = 'captured' }
}

function Graph-Impact($repoPath, $captureFile, $take) {
    switch ($script:G.Kind) {
        'direct' {
            & $script:G.Exe impact --symbol AgentEvent.Validate --repo $repoPath --format text 2>&1 |
                Select-Object -First $take
        }
        'cli' {
            entire graph impact --symbol AgentEvent.Validate --repo $repoPath --format text 2>&1 |
                Select-Object -First $take
        }
        'captured' {
            Write-Host '  (Graph will not launch in this shell — showing the captured run)' -ForegroundColor DarkGray
            Get-Content (Join-Path $PSScriptRoot "demo-fallback\$captureFile") | Select-Object -First $take
        }
    }
}

# Section prints one named block of a rendered state. Select-String -Context
# loses its context lines across a pipe, so the boundary is found explicitly:
# a section runs until the next line that starts in column 0 and is upper case.
function Section($lines, $headers) {
    $out = @()
    $in = $false
    foreach ($line in $lines) {
        if ($headers -contains $line.Trim() -and $line -notmatch '^\s') {
            $in = $true; $out += $line; continue
        }
        if ($in) {
            if ($line -match '^[A-Z][A-Z ]{2,}$') { $in = $false; continue }
            $out += $line
        }
    }
    $out
}

function Beat($n, $title, $say) {
    Write-Host ''
    Write-Host ('=' * 74) -ForegroundColor DarkGray
    Write-Host "  $n  $title" -ForegroundColor Cyan
    if ($say) { Write-Host "  > $say" -ForegroundColor DarkYellow }
    Write-Host ('=' * 74) -ForegroundColor DarkGray
    Write-Host ''
}

function Next() {
    Write-Host ''
    Write-Host '  [Enter to continue]' -ForegroundColor DarkGray
    [void](Read-Host)
}

# --------------------------------------------------------------- warm-up mode
if ($Warm) {
    Write-Host 'Building binaries...' -ForegroundColor Cyan
    go build -o entire-continuity.exe ./cmd/entire-continuity
    go build -o entire-agent-acmecode.exe ./cmd/entire-agent-acmecode

    Write-Host 'Seeding the task...' -ForegroundColor Cyan
    Remove-Item -Recurse -Force .entire-continuity -ErrorAction SilentlyContinue
    .\entire-continuity.exe ingest --file $fixture --quiet | Out-Null

    $script:G = Resolve-Graph
    Write-Host ("Graph mode: " + $script:G.Kind) -ForegroundColor Cyan
    if ($script:G.Kind -eq 'captured') {
        Write-Host '  Graph will not launch here; the demo will show the captured run.' -ForegroundColor Yellow
    } else {
        Write-Host 'Warming the Graph index (the slow part, ~30s)...' -ForegroundColor Cyan
        Graph-Impact $root  'graph-repo-root.txt'   6 | Out-Null
        Graph-Impact $agent 'graph-module-root.txt' 9 | Out-Null
    }

    Write-Host ''
    Write-Host 'Ready. Run .\demo.ps1 to present.' -ForegroundColor Green
    exit 0
}

# ------------------------------------------------------------------- the demo
$script:G = Resolve-Graph

# Reset and re-seed silently, so the demo is repeatable. Without this, a second
# run starts with the human decisions already recorded from the first — and
# beat 3 would show them before beat 4 adds them, spoiling the whole reveal.
Remove-Item -Recurse -Force .entire-continuity -ErrorAction SilentlyContinue
.\entire-continuity.exe ingest --file $fixture --quiet | Out-Null

Clear-Host
Write-Host ''
Write-Host '  ENTIRE CONTINUITY' -ForegroundColor White
Write-Host '  The agent changes. The task does not.' -ForegroundColor DarkGray
Write-Host ''
Write-Host '  An agent works a task for twenty minutes. The session ends. A new'
Write-Host '  agent picks it up and re-derives an approach the first one already'
Write-Host '  proved broken. Git shows what the code IS, never what was tried'
Write-Host '  and rejected.'
Next

Beat '1' 'THE INPUT' 'A runtime we had never heard of, in a format we had never seen.'
Get-Content $fixture -TotalCount 3
Next

Beat '2' 'INGEST' 'Format detected per record. I never told it which.'
.\entire-continuity.exe ingest --file $fixture --quiet
Next

Beat '3' 'THE ENGINEERING STATE' 'Intent recovered. Every gap named. Every claim carries its confidence.'
.\entire-continuity.exe task status $task
Next

Beat '4' 'A HUMAN ADDS WHAT NO TRANSCRIPT CONTAINS' 'The two facts a diff can never recover.'
Write-Host '  A reviewer reads the state and records why an approach failed.' -ForegroundColor DarkGray
Write-Host ''
Write-Host '  $ task decide  --agent human' -ForegroundColor DarkGray
.\entire-continuity.exe task decide $task --agent human --session human_review `
    --decision "Evaluate expiry before the enabled check" `
    --reason   "Checking enabled first made the error non-deterministic for an expired-and-disabled coupon" 2>&1 |
    Select-Object -First 5
Write-Host ''
Write-Host '  $ task reject  --agent human  --evidence apply_coupon.test.ts' -ForegroundColor DarkGray
.\entire-continuity.exe task reject $task --agent human --session human_review `
    --approach "Return the first failing validation in declaration order" `
    --reason   "Order was implementation-defined, so the same coupon produced different errors between runs" `
    --evidence "apply_coupon.test.ts" 2>&1 | Select-Object -First 10
Next

Beat '5' 'IT IS NOW PART OF THE TASK' 'Cited, levelled, and carried by every future merge.'
$state = .\entire-continuity.exe task status $task 2>&1
Section $state @('DECISIONS', 'REJECTED APPROACHES')
Next

Beat '6' 'THE WORKER CHANGES' 'A different runtime picks the task up. Watch the task id.'
Write-Host '  $ task resume --agent hermes' -ForegroundColor DarkGray
.\entire-continuity.exe task resume $task --session hermes_cont_01 --agent hermes 2>&1 | Select-Object -First 8
Write-Host ''
Write-Host '  $ task handoff --from acmecode --to hermes --record' -ForegroundColor DarkGray
.\entire-continuity.exe task handoff $task --from-session btw-track3-demo-001 --from-agent acmecode `
    --to-session hermes_cont_01 --to-agent hermes --record 2>&1 | Out-Null
.\entire-continuity.exe task lineage $task
Next

Beat '7' 'GRAPH, FROM THE REPOSITORY ROOT' 'One caller. And it is a test.'
Graph-Impact $root 'graph-repo-root.txt' 6
Next

Beat '8' 'GRAPH, FROM THE MODULE ROOT' 'Same symbol. Same command. Only --repo changed.'
Graph-Impact $agent 'graph-module-root.txt' 9
Next

Beat '9' 'THE SOURCE CONFIRMS' 'Four production callers, in the three packages the design rested on.'
Select-String -Path internal\continuity\*\*.go -Pattern '\.Validate\(\)' |
    Where-Object { $_.Path -notlike '*_test.go' } |
    ForEach-Object { '  {0}:{1}' -f (Resolve-Path -Relative $_.Path), $_.LineNumber }
Next

Beat '10' 'THE CURVEBALL, AND THE TEST THAT PROVES IT' 'Old fixtures decode byte-identically through both paths.'
go test ./internal/continuity/normalize/ -run 'TestOriginalFormatsStillDecode|TestUnknownEventsAreRetained|TestIncompleteTranscript' -v 2>&1 |
    Select-String -Pattern '^(--- )?(PASS|ok)'

Write-Host ''
Write-Host ('=' * 74) -ForegroundColor DarkGray
Write-Host '  The agent changed. The task did not.' -ForegroundColor White
Write-Host ('=' * 74) -ForegroundColor DarkGray
Write-Host ''
