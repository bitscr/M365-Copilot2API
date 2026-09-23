#!/usr/bin/env bash
# sandbox-sync.sh — run INSIDE a sandbox/container whose tool env is NOT the
# M365 host. Bridges the control plane to the host's auto-deploy timer:
#   1. ensures a working clone of the repo exists here
#   2. pulls latest origin/main
#   3. (you edit files here, then) `git push origin main`
# The host timer picks up the push within ~2 minutes and tests/builds/swaps/
# restarts itself. Never touch /root/m365-panel on a box where it doesn't
# exist — the repo lives in this script's CWD instead.
#
# Credentials: pushes need SSH write access to github.com (origin).
# If ~/.ssh has no usable key, the script fails loudly with instructions
# instead of pretending.

set -uo pipefail

REPO_DIR="${1:-$(pwd)/m365-panel}"
BRANCH=main

echo "== sandbox-sync: repo at $REPO_DIR"

if [ ! -d "$REPO_DIR/.git" ]; then
    echo "== repo missing, cloning..."
    git clone git@github.com:bitscr/M365-Copilot2API.git "$REPO_DIR" || {
        echo "!! clone failed. Check SSH access to github.com:"
        echo "   ssh -T git@github.com"
        echo "   (needs a key that can write bitscr/M365-Copilot2API;"
        echo "    a read-only key can fetch but NOT push — deploy needs write)"
        exit 1
    }
fi

cd "$REPO_DIR" || exit 1

echo "== pulling origin/$BRANCH"
git fetch origin "$BRANCH" || { echo "!! fetch failed"; exit 1; }
git checkout "$BRANCH" 2>/dev/null || true
git merge --ff-only "origin/$BRANCH" || {
    echo "!! local edits conflict with origin; commit or stash them first"
    exit 1
}

echo "== ready. HEAD: $(git rev-parse --short HEAD)"
echo "   edit files, then:  git add -A && git commit -m 'fix(...)' && git push origin $BRANCH"
echo "   host auto-deploy log: cat /root/m365-data/autodeploy.log (on the host)"
