# actl — VHS demo tape
# ---------------------------------------------------------------------------
# Render:   vhs demo/demo.tape          (run from the repo root)
# Output:   demo/demo.gif
#
# Before recording, make the run deterministic:
#   1. Build the binary onto PATH:   go build -o "$HOME/bin/actl" ./cmd/actl
#      (or change the command below to:  go run ./cmd/actl testdata/workflows/demo.yml)
#   2. Pre-pull the runner image so the first step doesn't stall the timings:
#        docker pull catthehacker/ubuntu:act-latest
#
# The default -source is the current dir, so the intercepted checkout copies the
# actl working tree itself — that's why `ls -la` in the shell shows this repo. If
# the copy feels slow (the act submodule under third_party/ is sizeable), bump the
# Sleep after the checkout step. Sleeps assume the image is already local.
# ---------------------------------------------------------------------------
Output demo/demo.webm
Output demo/demo.mp4
Output demo/demo.gif

Require actl
Require docker

Set Shell "bash"
Set FontSize 18
Set Width 1280
Set Height 760
Set Padding 18
Set Theme "Catppuccin Mocha"
Set TypingSpeed 20ms
# vi is what `i` (edit-cmd) spawns; keep it explicit so the edit beat is reproducible.
Env EDITOR "vi"

# ── Beat 0 — the pain, in one line ─────────────────────────────────────────
Type "# debug a GitHub Actions workflow locally — pause, inspect, re-run"
Sleep 1500ms
Enter
Sleep 400ms

# ── Beat 1 — launch, paused BEFORE checkout (empty workspace, like on GitHub)─
Type "actl testdata/workflows/demo.yml"
Sleep 800ms
Enter
# startup + container bring-up (image pre-pulled)
Sleep 5s
Sleep 1500ms

# ── Beat 2 — step past the intercepted checkout ────────────────────────────
# actl rewrites actions/checkout to a no-op and copies your working tree in at
# this step's position — before it the workspace is empty, after it your code is there.
Type "s"
Sleep 3s

# ── Beat 3 — peek at the live env, then drop into the container ─────────────
Type "e"
Sleep 2500ms
Type "e"
Sleep 600ms

# ── Beat 4 — THE moment: a shell in the live container, mid-workflow ────────
Type "d"
Sleep 2s
# it's the real workspace — your actual repo (this is the actl repo itself):
Type "ls -la"
Sleep 600ms
Enter
Sleep 2500ms
Type "cat go.mod | head -1"
Sleep 600ms
Enter
Sleep 1800ms
Type "exit"
Sleep 400ms
Enter
Sleep 1500ms

# ── Beat 5 — step onto `build`, edit its command, re-run it in place ────────
Type "s"
Sleep 1500ms
# run `build`, so it's paused AFTER it and re-run is unlocked
Type "s"
Sleep 2s
# edit the step command ($EDITOR=vi opens on the script):
Type "i"
Sleep 1500ms
# vi: append to the end of the line, then save
Type "A"
Sleep 400ms
Type " && echo ✦ patched live"
Sleep 600ms
Escape
Sleep 400ms
Type ":wq"
Sleep 300ms
Enter
Sleep 1200ms
# re-run the step with the edit — no git push, no re-trigger
Type "r"
Sleep 2500ms

# ── Beat 6 — let it finish, clean green run ────────────────────────────────
Type "c"
Sleep 3s
Type "q"
Sleep 800ms
