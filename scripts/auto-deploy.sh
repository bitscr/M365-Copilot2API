#!/usr/bin/env bash
# m365 auto-deploy: pull origin/main, test, build, atomic swap, restart, verify.
# Runs from systemd timer every 2 minutes. No-op when already up to date.
# Fits the control-plane/execution-plane split: fixes pushed to origin/main
# from ANY environment (host, sandbox, another box) deploy themselves here.

set -uo pipefail

REPO=/root/m365-panel
DATA=/root/m365-data
LOG="$DATA/autodeploy.log"
GO=/usr/local/go/bin/go
SVC=m365-api.service

log() { printf '%s %s\n' "$(date '+%F %T')" "$*" >>"$LOG"; }

cd "$REPO" || { log "ERROR: repo missing at $REPO"; exit 1; }

# Preconditions: clean tree, otherwise never force through local edits.
if ! git diff --quiet; then
    log "SKIP: working tree dirty; refusing auto-deploy over local edits"
    exit 0
fi

git fetch origin main --quiet 2>>"$LOG" || { log "ERROR: git fetch failed"; exit 1; }

HEAD_LOCAL=$(git rev-parse HEAD)
HEAD_REMOTE=$(git rev-parse origin/main)
if [ "$HEAD_LOCAL" = "$HEAD_REMOTE" ]; then
    exit 0 # up to date, silent no-op
fi

log "DEPLOY: $HEAD_LOCAL -> $HEAD_REMOTE ($(git log --oneline -1 origin/main))"

git merge --ff-only origin/main >>"$LOG" 2>&1 || { log "ERROR: ff merge failed"; exit 1; }

export PATH=/usr/local/go/bin:$PATH

# Test gate: never ship a red suite.
if ! "$GO" test ./... >/tmp/m365-test.log 2>&1; then
    log "ERROR: tests failed, staying on $(git rev-parse HEAD^)"
    git reset --hard HEAD^ >>"$LOG" 2>&1
    tail -5 /tmp/m365-test.log >>"$LOG"
    exit 1
fi

# Build gate: build to temp; only swap on success.
if ! "$GO" build -o m365-copilot2api.new ./cmd/server >/tmp/m365-build.log 2>&1; then
    log "ERROR: build failed, staying on $(git rev-parse HEAD^)"
    git reset --hard HEAD^ >>"$LOG" 2>&1
    tail -5 /tmp/m365-build.log >>"$LOG"
    exit 1
fi

STAMP=$(date +%Y%m%d-%H%M%S)
cp -a m365-copilot2api "m365-copilot2api.bak-$STAMP" >>"$LOG" 2>&1
mv m365-copilot2api.new m365-copilot2api
chmod 755 m365-copilot2api

if ! systemctl restart "$SVC" >>"$LOG" 2>&1; then
    log "ERROR: restart failed"
    exit 1
fi

sleep 2

if [ "$(systemctl is-active "$SVC")" != "active" ]; then
    log "ERROR: service not active after deploy"
    exit 1
fi

log "OK: deployed $(git rev-parse HEAD), service active"
