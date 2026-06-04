#!/usr/bin/env bash
# Builds a small, local upstream + fork pair for trying the
# upstream-rebase plugin end-to-end without touching a real remote.
#
#   plugin-upstream-rebase/hack/make-fixture.sh [dir]
#
# Creates (default dir: ${TMPDIR:-/tmp}/rebase-fixture):
#   upstream.git  tags v1.0, v1.1 (v1.1 changes line 2 of a.txt and
#                 contains "PR 777")
#   fork.git      master = previous `merge -s ours` rebase onto v1.0 plus:
#                   UPSTREAM: <carry>  conflicts with v1.1 → needs a resolution
#                   UPSTREAM: <carry>  clean
#                   UPSTREAM: <drop>   dropped
#                   UPSTREAM: 777      already upstream in v1.1 → auto-skipped
#                   (no prefix)        → needs a carry/drop decision
#   push.git      empty bare repo the rebase is pushed to
#
# Then writes rebase-fixture.workflow.yaml (copy it into the
# orchestrator's --workflows dir) and prints what to do next.
set -euo pipefail

DIR="${1:-${TMPDIR:-/tmp}/rebase-fixture}"
DIR="${DIR%/}"
rm -rf "$DIR"
mkdir -p "$DIR"
cd "$DIR"

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=Dev GIT_AUTHOR_EMAIL=dev@example.com
export GIT_COMMITTER_NAME=Dev GIT_COMMITTER_EMAIL=dev@example.com

commit() { # dir file content message
  mkdir -p "$(dirname "$1/$2")"
  printf '%b' "$3" > "$1/$2"
  git -C "$1" add -A
  git -C "$1" commit -q -m "$4"
}

git init -q -b master upstream-wt
commit upstream-wt a.txt 'line1\nline2\nline3\n' "base"
commit upstream-wt Makefile 'update:\n\techo generated > generated.txt\n' "upstream Makefile"
git -C upstream-wt tag -a -m v1.0 v1.0
commit upstream-wt a.txt 'line1\nUPSTREAM CHANGED\nline3\n' "upstream changes line2"
commit upstream-wt pr.txt 'pr content\n' "upstream merges PR 777"
git -C upstream-wt tag -a -m v1.1 v1.1
git clone -q --bare upstream-wt upstream.git

git clone -q upstream.git fork-wt
git -C fork-wt checkout -q -b old v1.0~1
commit fork-wt stale.txt 'stale\n' "UPSTREAM: <carry>: stale carry from an older rebase (must NOT be picked)"
git -C fork-wt checkout -q -B master v1.0
git -C fork-wt merge -q -s ours --no-edit old
commit fork-wt a.txt 'line1\nFORK CHANGED\nline3\n' "UPSTREAM: <carry>: fork changes line2"
commit fork-wt clean.txt 'clean\n' "UPSTREAM: <carry>: clean carry"
commit fork-wt generated.txt 'old generated\n' "UPSTREAM: <drop>: make update"
commit fork-wt pr.txt 'pr content\n' "UPSTREAM: 777: carried upstream PR"
commit fork-wt misc.txt 'misc\n' "fix a thing without a prefix"
git clone -q --bare fork-wt fork.git
git -C fork.git branch -D old >/dev/null
git init -q --bare push.git

# Workflows are defined only by the YAML files in the orchestrator's
# --workflows directory, so the fixture is a workflow file to copy there.
cat > rebase-fixture.workflow.yaml <<YAML
id: rebase-fixture
display_name: "Rebase fixture (local)"
steps:
  - id: rebase
    plugin: upstream-rebase
    instance: rebase-bot
    action: rebase
    timeout: 15m
    params:
      clone_url: "$DIR/fork.git"
      fork_branch: master
      upstream_url: "$DIR/upstream.git"
      target_ref: v1.1
      push_url: "$DIR/push.git"
      rerere: suggest
      dirs:
        - path: "."
          make_targets: [update]
YAML

cat <<MSG
Fixture ready in $DIR

1. Create an Upstream Rebase instance (Instances → Upstream Rebase →
   New instance; any valid unencrypted SSH private key works - local
   file paths don't use it). The UI gives it a generated ID, shown under
   its name in the instance list.
2. Register the workflow: copy it into the orchestrator's --workflows
   directory, replace "instance: rebase-bot" with that ID, and restart
   the orchestrator.
     cp $DIR/rebase-fixture.workflow.yaml <repo>/workflows/
   (Delete the copy when you're done; the next restart then flags the
   workflow removed, and you can purge it from its page.)
3. Workflows → "Rebase fixture (local)" → Run now. Expect:
     - a "Needs input" card asking carry/drop for "fix a thing without a prefix"
     - then a conflict on a.txt to resolve
     - then success; inspect with: git -C $DIR/push.git log --oneline rebase/v1-1
4. Run it again: the decision and resolution are reused, no input needed.
MSG
