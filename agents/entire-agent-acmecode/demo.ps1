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

Beat '4' 'GRAPH, FROM THE REPOSITORY ROOT' 'One caller. And it is a test.'
Graph-Impact $root 'graph-repo-root.txt' 6
Next

Beat '5' 'GRAPH, FROM THE MODULE ROOT' 'Same symbol. Same command. Only --repo changed.'
Graph-Impact $agent 'graph-module-root.txt' 9
Next

Beat '6' 'THE SOURCE CONFIRMS' 'Four production callers, in the three packages the design rested on.'
Select-String -Path internal\continuity\*\*.go -Pattern '\.Validate\(\)' |
    Where-Object { $_.Path -notlike '*_test.go' } |
    ForEach-Object { '  {0}:{1}' -f (Resolve-Path -Relative $_.Path), $_.LineNumber }
Next

Beat '7' 'THE CURVEBALL, AND THE TEST THAT PROVES IT' 'Old fixtures decode byte-identically through both paths.'
go test ./internal/continuity/normalize/ -run 'TestOriginalFormatsStillDecode|TestUnknownEventsAreRetained|TestIncompleteTranscript' -v 2>&1 |
    Select-String -Pattern '^(--- )?(PASS|ok)'

Write-Host ''
Write-Host ('=' * 74) -ForegroundColor DarkGray
Write-Host '  The agent changed. The task did not.' -ForegroundColor White
Write-Host ('=' * 74) -ForegroundColor DarkGray
Write-Host ''
