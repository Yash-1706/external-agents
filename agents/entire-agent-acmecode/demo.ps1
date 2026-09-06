# Entire Continuity — demo driver.
#
#   .\demo.ps1 -Warm     run once before presenting; builds binaries and warms
#                        the Graph index so nothing pauses in front of judges
#   .\demo.ps1           the demo; press Enter between beats
#
# Graph is invoked through the plugin binary directly rather than through
# "entire graph", because the CLI's plugin dispatch does not resolve in every
# shell. Same tool, same output, one less thing to fail live.

param([switch]$Warm)

$ErrorActionPreference = 'Continue'
$root  = 'D:\entire\external-agents'
$agent = Join-Path $root 'agents\entire-agent-acmecode'
$graph = Join-Path $env:LOCALAPPDATA 'entire\plugins\bin\entire-graph.exe'
$fixture = 'internal\continuity\normalize\testdata\lifecycle_v2_acmecode.jsonl'
$task  = 'task_fb86c4d96b3163f1'

Set-Location $agent

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

    Write-Host 'Warming the Graph index (this is the slow part, ~30s)...' -ForegroundColor Cyan
    & $graph impact --symbol AgentEvent.Validate --repo $root   --format text | Out-Null
    & $graph impact --symbol AgentEvent.Validate --repo $agent  --format text | Out-Null

    Write-Host ''
    Write-Host 'Ready. Run .\demo.ps1 to present.' -ForegroundColor Green
    exit 0
}

# ------------------------------------------------------------------- the demo
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
& $graph impact --symbol AgentEvent.Validate --repo $root --format text 2>&1 | Select-Object -First 6
Next

Beat '5' 'GRAPH, FROM THE MODULE ROOT' 'Same symbol. Same command. Only --repo changed.'
& $graph impact --symbol AgentEvent.Validate --repo $agent --format text 2>&1 | Select-Object -First 9
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
