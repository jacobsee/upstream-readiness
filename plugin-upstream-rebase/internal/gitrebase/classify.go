// Package gitrebase is the git mechanics of the upstream-rebase plugin,
// kept free of gRPC / storage so it can be tested against scratch repos:
// classifying fork commits, locating the previous `merge -s ours`
// rebase point, enumerating every commit that must be considered,
// cherry-picking with outcome detection, conflict capture /
// fingerprinting / resolution, and rerere cache import / export.
package gitrebase

import (
	"regexp"
	"strconv"
	"strings"
)

// Kind classifies a fork commit found since the previous rebase.
type Kind string

const (
	// KindCarry - `UPSTREAM: <carry>: …`: cherry-pick onto every rebase.
	KindCarry Kind = "carry"
	// KindDrop - `UPSTREAM: <drop>: …`: never picked (typically
	// generated content the post-pick make targets regenerate).
	KindDrop Kind = "drop"
	// KindPR - `UPSTREAM: <12345>: …`: an upstream PR carried until
	// upstream absorbs it. Picked; becoming empty means it landed.
	KindPR Kind = "pr"
	// KindMalformed - starts with "UPSTREAM" but doesn't parse. Needs a
	// human decision; guessing is how patches get lost.
	KindMalformed Kind = "malformed"
	// KindUnclassified - no UPSTREAM prefix at all. Needs a decision.
	KindUnclassified Kind = "unclassified"
	// KindEvilMerge - a merge commit whose result differs from all its
	// parents (`git diff-tree --cc` non-empty). Its content lives only
	// in the merge and would be silently lost; needs acknowledgement.
	KindEvilMerge Kind = "evil_merge"
	// KindQueuedPatch - a commit the plugin's own patch queue produced
	// on a previous rebase (carries the PatchTrailer). Not picked: the
	// queue re-applies it after the dependency phase.
	KindQueuedPatch Kind = "queued_patch"
)

// PatchTrailer marks commits produced from the per-step patch queue.
const PatchTrailer = "Rebase-Patch-Id"

// upstreamRe accepts the conventional `UPSTREAM: <carry>: `,
// `UPSTREAM: <drop>: `, `UPSTREAM: 12345: ` forms plus the unbracketed /
// bracketed variants (`carry`, `<12345>`). Anything else beginning with
// "UPSTREAM" is malformed.
var upstreamRe = regexp.MustCompile(`^UPSTREAM: (?:<(carry|drop)>|(carry|drop)|<(\d+)>|(\d+)): \S`)

// Classify returns the commit's kind and, for KindPR, the PR number.
// patchID is the commit's PatchTrailer value ("" if none); it wins over
// the subject because a queued mbox patch may carry any message.
func Classify(subject, patchID string) (Kind, int) {
	if patchID != "" {
		return KindQueuedPatch, 0
	}
	m := upstreamRe.FindStringSubmatch(subject)
	if m == nil {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(subject)), "UPSTREAM") {
			return KindMalformed, 0
		}
		return KindUnclassified, 0
	}
	switch {
	case m[1] == "carry" || m[2] == "carry":
		return KindCarry, 0
	case m[1] == "drop" || m[2] == "drop":
		return KindDrop, 0
	case m[3] != "":
		n, _ := strconv.Atoi(m[3])
		return KindPR, n
	default:
		n, _ := strconv.Atoi(m[4])
		return KindPR, n
	}
}

// NeedsClassification reports whether a commit of this kind requires a
// human decision before the rebase can proceed.
func NeedsClassification(k Kind) bool {
	return k == KindMalformed || k == KindUnclassified || k == KindEvilMerge
}
