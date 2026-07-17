package consensus

import (
	"bytes"
	"sort"

	"BluePods/internal/types"
)

// anchorKind enumerates the three outcomes of the anchor decision rule.
type anchorKind int

const (
	anchorWait   anchorKind = iota // anchorWait: no verdict yet; the caller must retry later
	anchorCommit                   // anchorCommit: the anchor is decided committed; anchor is set
	anchorSkip                     // anchorSkip: the anchor is decided skipped; no batch this round
)

// anchorDecision is one round's verdict from the anchor rule. anchor holds the
// resolved vertex whose causal history forms the commit batch, meaningful only
// when kind is anchorCommit.
type anchorDecision struct {
	kind   anchorKind // kind is commit, skip, or wait
	anchor Hash       // anchor is the resolved vertex (valid only for anchorCommit)
}

// anchorVerdict is the DIRECT (round-N+1 citation) verdict for a single round's
// designated anchor, before the indirect rule is consulted.
type anchorVerdict int

const (
	verdictWait      anchorVerdict = iota // verdictWait: the round-N+1 stake set is unavailable
	verdictUndecided                      // verdictUndecided: no citation quorum formed either way
	verdictCertified                      // verdictCertified: one producer vertex reached the citation quorum
	verdictBlamed                         // verdictBlamed: the no-citation quorum formed
)

// citationTally groups round-N+1 producers by whom they cite among the designated
// producer's round-N vertices.
type citationTally struct {
	supporters map[Hash]map[Hash]bool // supporters[vertex] is the set of round-N+1 producers citing that vertex
	blamers    map[Hash]bool          // blamers is the set of round-N+1 producers that cite no vertex of the producer
}

// anchorStatus decides a round's anchor: commit with the resolved vertex, skip, or
// wait. The round's own direct verdict decides it when certified (commit) or
// blamed (skip). When it is undecided, the indirect rule scans forward for the
// FIRST later CERTIFIED anchor and decides the round by whether its producer sits
// in that anchor's causal history. Later blamed rounds are decided-skips and are
// scanned past. A later UNDECIDED round normally returns wait, because it could
// still certify and become the resolving anchor — resolving past it would risk a
// view-dependent, forking result. The one exception is a later undecided round
// whose designated anchor is CERTIFICATION-IMPOSSIBLE (anchorCertImpossible): such
// a round can never certify on any node, so it can never be the resolving anchor,
// and the scan passes it to reach the genuinely certified anchor beyond. Without
// that exception a run of permanently-undecided rounds (an anchor producer that was
// unreachable when its round passed) wedges the commit cursor forever even though a
// later certified anchor would resolve it. An unavailable stake set still returns
// wait. Wait therefore holds until every round up to the resolving anchor is either
// decided or provably cert-impossible.
func (d *DAG) anchorStatus(round uint64) anchorDecision {
	latest := d.store.highestRound()

	for r := round; r <= latest; r++ {
		verdict, vertex := d.directAnchorVerdict(r)

		switch {
		case verdict == verdictCertified && r == round:
			return anchorDecision{kind: anchorCommit, anchor: vertex}
		case verdict == verdictCertified:
			return d.resolveIndirect(round, vertex)
		case verdict == verdictBlamed && r == round:
			return anchorDecision{kind: anchorSkip}
		case verdict == verdictBlamed:
			continue // a later decided-skip is never the anchor; keep scanning
		case verdict == verdictUndecided && r == round:
			continue // round itself is undecided; scan forward for a resolver
		case verdict == verdictUndecided && d.anchorCertImpossible(r):
			continue // a later round that can never certify is never the resolving anchor
		default:
			return anchorDecision{kind: anchorWait} // later-undecided-but-still-certifiable or unavailable: cannot resolve past it
		}
	}

	return anchorDecision{kind: anchorWait}
}

// directAnchorVerdict computes the strict direct verdict for a round's designated
// anchor from round-N+1 citations only. A producer vertex cited by a round-N+1
// stake quorum is certified; a round-N+1 stake quorum citing no vertex of the
// producer blames the round; anything else is undecided. It reads round-N+1 DIRECT
// parents only, never transitive reachability, so a lone small supporter (or a
// round-N+2 vertex) cannot manufacture a quorum. It waits when the designated
// producer or the round-N+1 stake set does not resolve yet. This is the unit the
// relaxed-bootstrap regime (Task 0.5) wraps.
func (d *DAG) directAnchorVerdict(round uint64) (anchorVerdict, Hash) {
	producer, ok := d.anchorProducerFor(round)
	if !ok {
		return verdictWait, Hash{}
	}

	set, ok := d.HoldersForEpoch(d.commitEpochForRound(round + 1))
	if !ok {
		return verdictWait, Hash{}
	}

	return d.verdictFromTally(set, d.tallyCitations(round, producer), d.roundIsRelaxed(round))
}

// tallyCitations classifies each round-N+1 producer as a supporter of every
// designated-producer vertex it cites among its direct parents, or as a blamer
// when it cites none. A producer that only cites a different vertex of the
// designated producer supports that other vertex and is never a blamer: it
// abstains for the vertices it did not cite, so abstention never feeds blame.
func (d *DAG) tallyCitations(round uint64, producer Hash) citationTally {
	candidates := d.designatedVertexSet(round, producer)
	cited := d.citationsByProducer(round+1, candidates)

	tally := citationTally{
		supporters: make(map[Hash]map[Hash]bool),
		blamers:    make(map[Hash]bool),
	}

	for p, votes := range cited {
		if len(votes) == 0 {
			tally.blamers[p] = true
			continue
		}
		for v := range votes {
			addSupporter(tally.supporters, v, p)
		}
	}

	return tally
}

// designatedVertexSet returns the designated producer's round-N vertices as a set
// for fast citation lookup. It is empty when the producer produced nothing, which
// drives every round-N+1 producer into the blamers.
func (d *DAG) designatedVertexSet(round uint64, producer Hash) map[Hash]bool {
	hashes, _ := d.store.getByRoundProducer(round, producer)

	set := make(map[Hash]bool, len(hashes))
	for _, h := range hashes {
		set[h] = true
	}

	return set
}

// citationsByProducer maps each producer at the round to the set of candidate
// vertices it cites among its direct parents. A producer that produced a vertex in
// the round but cites no candidate maps to an empty set, marking it a blamer. An
// equivocating producer's citations are unioned across its vertices, so it blames
// only when none of its vertices cites any candidate.
func (d *DAG) citationsByProducer(round uint64, candidates map[Hash]bool) map[Hash]map[Hash]bool {
	result := make(map[Hash]map[Hash]bool)

	for _, hash := range d.store.getByRound(round) {
		vertex := d.store.get(hash)
		if vertex == nil {
			continue
		}

		producer := extractProducer(vertex)
		votes := result[producer]
		if votes == nil {
			votes = make(map[Hash]bool)
			result[producer] = votes
		}

		collectCitedCandidates(vertex, candidates, votes)
	}

	return result
}

// collectCitedCandidates adds every direct parent of vertex that is a candidate to
// the votes set.
func collectCitedCandidates(vertex *types.Vertex, candidates, votes map[Hash]bool) {
	for _, parent := range appendParentHashes(nil, vertex) {
		if candidates[parent] {
			votes[parent] = true
		}
	}
}

// verdictFromTally reduces a citation tally to a verdict over the round-N+1 holder
// set. Certified is tested first, hash-ascending, so the candidate is selected by
// the citations themselves — never by a local view — which keeps an equivocating
// producer from forking the verdict. In the STRICT regime certified needs the 2/3
// capped-stake quorum and blamed needs it too; the two are mutually exclusive since
// two 2/3 quorums over one stake set intersect. In the RELAXED bootstrap regime a
// single committed supporter certifies (the existing single-producer certificate),
// and the round is NEVER directly blamed: a thin blame quorum would fork against a
// peer that certified the vote-determined candidate on a delivery gap (I1), so an
// absent producer is skipped by the indirect rule instead.
func (d *DAG) verdictFromTally(set *ValidatorSet, tally citationTally, relaxed bool) (anchorVerdict, Hash) {
	for _, v := range sortedHashKeys(tally.supporters) {
		if d.certifies(set, tally.supporters[v], relaxed) {
			return verdictCertified, v
		}
	}

	if !relaxed && d.reachesStrictQuorum(set, tally.blamers) {
		return verdictBlamed, Hash{}
	}

	return verdictUndecided, Hash{}
}

// certifies reports whether a set of supporters certifies its vertex. The strict
// regime requires the 2/3 capped-stake quorum over the capped total. The relaxed
// bootstrap regime accepts a single supporter that is a member of the holder set,
// the existing single-producer certificate, so bootstrap converges before stake
// weighting is authoritative; membership (not stake) is used so a not-yet-bonded
// joiner can still certify.
func (d *DAG) certifies(set *ValidatorSet, producers map[Hash]bool, relaxed bool) bool {
	if relaxed {
		return membersInSet(set, producers) >= 1
	}

	return d.reachesStrictQuorum(set, producers)
}

// reachesStrictQuorum reports whether a set of producers carries the 2/3 capped
// stake quorum within the holder snapshot, dividing the capped sum by the capped
// total. Producers outside the snapshot contribute zero, so unknown or non-member
// citers cannot manufacture a quorum.
func (d *DAG) reachesStrictQuorum(set *ValidatorSet, producers map[Hash]bool) bool {
	cappedSum, cappedTotal := d.cappedStakeOf(set, producers)

	return quorumReached(cappedSum, cappedTotal)
}

// anchorCertImpossible reports whether an UNDECIDED later round can NEVER be
// certified, so anchorStatus's forward scan may pass it to reach a genuinely
// certified resolving anchor instead of waiting on it forever. It is the disarming
// predicate for the permanent commit wedge: a run of undecided rounds (an anchor
// producer unreachable, or dead, when its round passed) otherwise blocks the scan
// from ever reaching the certified anchor that would resolve the wedged round.
//
// It fires only in the STRICT regime (a relaxed bootstrap round certifies on a
// single supporter and is never ruled out here) and only when the round-N+1 holder
// snapshot resolves. The rule is potential support: a holder is a BLAMER only when
// it has at least one STORED round-N+1 vertex and none of its stored round-N+1
// vertices cites any of the candidate's vertices — the SAME union rule the direct
// verdict uses (tallyCitations), so a producer that ever cites a candidate, even
// via an equivocating second vertex, is a possible supporter, never a blamer. Every
// holder with NO stored round-N+1 vertex counts as a POTENTIAL SUPPORTER: its
// future vertex might cite the candidate, so assuming support is the conservative
// reading. The round is impossible when even the maximum achievable support falls
// below the strict 2/3 capped-stake quorum.
//
// Determinism, monotonicity, and zero rollback: the verdict is computed only from
// stored, immutable vertices (citations are parent hashes fixed at production), so
// any two nodes holding the same vertices compute the identical result. An honest
// holder produces at most one round-N+1 vertex, so as honest vertices arrive an
// absent holder either confirms the support already assumed for it or becomes a
// blamer — the achievable support only SHRINKS. Once it is below the quorum it
// stays below it on every node, so a cert-impossible round can never later become
// the resolving anchor — which is what preserves zero rollback.
//
// A holder with NO stored round-N+1 vertex normally keeps its potential-supporter
// weight forever, which lets a crashed (or long-partitioned) validator block the
// predicate indefinitely: a frontier split thinned by dead stake can leave stored
// blame below one third while the true achievable support is under the quorum. The
// SILENCE rule closes that: once the store holds a deep span of rounds above the
// frontier (anchorSilenceSpanRounds) in which a holder produced NOTHING, that
// holder's stake also stops counting as potential support. The honest-model
// argument: an honest producer produces rounds sequentially and cites every stored
// parent, so a holder absent from the whole observed span either never produces
// the frontier round at all (crashed: genuinely non-supporting, and the span stays
// empty forever on every node) or later back-fills the span citing the candidates
// it holds — support that completes certifications consistent with the membership
// decisions other nodes took through the resolving anchor. A holder merely delayed
// by load is covered by the span's depth: its vertices arrive well inside it.
//
// Byzantine caveat: a producer that first blames and later EQUIVOCATES a second
// round-N+1 vertex citing the candidate leaves the blamer set under the union rule,
// growing the achievable support. An equivocator whose stake covers the gap below
// the quorum at the blocking frontier could therefore flip an impossibility one
// node has already acted on, while a fuller-evidence node still waits and later
// certifies. A revived validator that back-fills a silent span with SELECTIVE
// citations (not citing candidates it holds) has the same power. Both are excluded
// by the honest-failure model the observed wedges live in (crash and partition
// unreachability, no adversarial stake) — the same >1/3 adversary envelope under
// which certification's own quorum-intersection argument fails.
func (d *DAG) anchorCertImpossible(round uint64) bool {
	if d.roundIsRelaxed(round) {
		return false
	}

	producer, ok := d.anchorProducerFor(round)
	if !ok {
		return false
	}

	set, ok := d.HoldersForEpoch(d.commitEpochForRound(round + 1))
	if !ok {
		return false
	}

	tally := d.tallyCitations(round, producer)
	if d.certificationRuledOut(set, tally.blamers) {
		return true
	}

	silent := d.silentHolders(set, round+1)
	if len(silent) == 0 {
		return false
	}

	return d.certificationRuledOut(set, mergeMemberSets(tally.blamers, silent))
}

// anchorSilenceSpanRounds is the depth of stored rounds above a frontier that must
// be observed, with a holder absent from ALL of them, before that holder's stake
// stops counting as potential support in anchorCertImpossible. At the production
// cadence this spans several seconds, far beyond gossip delivery skew under load,
// so a merely-slow holder's vertices land inside the span while a crashed one
// leaves it empty forever.
const anchorSilenceSpanRounds = 20

// silentHolders returns the holders with no stored vertex at ANY round of the span
// [frontier, frontier+anchorSilenceSpanRounds]. It returns nil while the store's
// highest round has not reached the span's end: an unobserved span proves nothing,
// which keeps a shallow store (or a fresh wedge) reading every absent holder as a
// potential supporter. Honest production is sequential, so a holder with a stored
// vertex above the span but none inside it still reads as present only once one of
// its in-span vertices arrives.
func (d *DAG) silentHolders(set *ValidatorSet, frontier uint64) map[Hash]bool {
	horizon := frontier + anchorSilenceSpanRounds
	if d.store.highestRound() < horizon {
		return nil
	}

	seen := d.store.producersInRange(frontier, horizon)

	silent := make(map[Hash]bool)
	for _, v := range set.All() {
		if !seen[v.Pubkey] {
			silent[v.Pubkey] = true
		}
	}

	return silent
}

// certificationRuledOut reports whether even the UNION of all possible supporters —
// every round-N+1 holder except the given non-supporters — falls short of the
// strict 2/3 capped-stake quorum. The union's support is at least any single
// candidate vertex's support, so if the union cannot reach quorum, no single
// candidate can, and the round can never be certified. A zero-weight holder set is
// never ruled out (it is a degenerate, not a decided, frontier).
func (d *DAG) certificationRuledOut(set *ValidatorSet, nonSupporters map[Hash]bool) bool {
	nonSupportCapped, cappedTotal := d.cappedStakeOf(set, nonSupporters)
	if cappedTotal == 0 {
		return false
	}

	maxSupport := cappedTotal - nonSupportCapped

	return !quorumReached(maxSupport, cappedTotal)
}

// mergeMemberSets returns the union of two member sets without mutating either.
func mergeMemberSets(a, b map[Hash]bool) map[Hash]bool {
	merged := make(map[Hash]bool, len(a)+len(b))
	for k := range a {
		merged[k] = true
	}
	for k := range b {
		merged[k] = true
	}

	return merged
}

// membersInSet counts how many of the producers are members of the holder set.
// Non-members (unknown or non-holder citers) never count toward a relaxed quorum.
func membersInSet(set *ValidatorSet, producers map[Hash]bool) int {
	count := 0
	for _, v := range set.All() {
		if producers[v.Pubkey] {
			count++
		}
	}

	return count
}

// resolveIndirect decides an undecided round from a later certified anchor: the
// round commits the lowest-hash vertex of its designated producer that sits in the
// anchor's causal history, and skips when none does. A certified anchor's causal
// history is agreed across nodes, so the resolution is deterministic; it waits when
// that history is not fully present locally.
func (d *DAG) resolveIndirect(round uint64, certified Hash) anchorDecision {
	producer, ok := d.anchorProducerFor(round)
	if !ok {
		return anchorDecision{kind: anchorWait}
	}

	candidates, ok := d.store.getByRoundProducer(round, producer)
	if !ok {
		return anchorDecision{kind: anchorSkip}
	}

	history, ok := d.store.causalBatch(certified)
	if !ok {
		return anchorDecision{kind: anchorWait}
	}

	return decideMembership(candidates, history)
}

// decideMembership commits the first candidate that appears in the anchor's causal
// history and skips when none appears. candidates are hash-ascending, so the first
// match is the lowest-hash vertex, keeping the choice deterministic.
func decideMembership(candidates, history []Hash) anchorDecision {
	inHistory := make(map[Hash]bool, len(history))
	for _, h := range history {
		inHistory[h] = true
	}

	for _, c := range candidates {
		if inHistory[c] {
			return anchorDecision{kind: anchorCommit, anchor: c}
		}
	}

	return anchorDecision{kind: anchorSkip}
}

// sortedHashKeys returns a map's keys in ascending byte order, so a scan over them
// is deterministic across nodes regardless of Go's map iteration order.
func sortedHashKeys(m map[Hash]map[Hash]bool) []Hash {
	keys := make([]Hash, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i][:], keys[j][:]) < 0
	})

	return keys
}

// addSupporter records producer p as a supporter of vertex v.
func addSupporter(supporters map[Hash]map[Hash]bool, v, p Hash) {
	set := supporters[v]
	if set == nil {
		set = make(map[Hash]bool)
		supporters[v] = set
	}

	set[p] = true
}
