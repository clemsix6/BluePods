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
// view-dependent, forking result. The one exception is a later undecided round that
// is CERTIFICATION-IMPOSSIBLE: such a round can never be the resolving anchor, and
// the scan passes it to reach the genuinely certified anchor beyond. Without that
// exception a run of permanently-undecided rounds (an anchor producer that was
// unreachable when its round passed) wedges the commit cursor forever even though a
// later certified anchor would resolve it.
//
// Impossibility has two grades. Blame — a citation quorum against the producer — is
// immutable: once a round is blame-impossible every node agrees, forever. Silence —
// a holder absent from a deep span counting as a non-supporter — is REVERSIBLE: a
// slow holder's vertices shrink the silent set, so a better-informed node may still
// certify a round a starved node called impossible. Passing a merely silence-
// impossible round is safe for any round two or more above the queried one, because
// quorum intersection forces the queried round's designated vertex into every anchor
// that far ahead, so all of them resolve it identically. The round IMMEDIATELY above
// the queried one is the sole exception: its certified anchor is one round up and can
// OMIT that vertex, resolving the queried round the opposite way. That round is passed
// on silence evidence only when its certification would agree with the later anchor;
// otherwise the scan waits for the missing vertices. An unavailable stake set still
// returns wait. Wait therefore holds until every round up to the resolving anchor is
// decided, blame-impossible, or a silence-impossible round whose certification could
// not change the outcome.
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
		case verdict == verdictUndecided:
			if dec, pass := d.scanPastUndecided(round, r); !pass {
				return dec
			}
			continue // a later round that cannot resolve `round` differently is safe to pass
		default:
			return anchorDecision{kind: anchorWait} // unavailable stake set: cannot resolve
		}
	}

	return anchorDecision{kind: anchorWait}
}

// scanPastUndecided decides whether the forward scan for an undecided queried round
// may pass a later undecided round r. It may pass only a certification-impossible r.
// Blame-impossibility is immutable and always safe to pass. Silence-impossibility is
// reversible, so passing r ON SILENCE is unsafe exactly when r is the round
// immediately above the queried one and its certification could resolve the queried
// round differently from the later anchor the scan will use; then it waits. The bool
// is true when the scan may continue past r.
func (d *DAG) scanPastUndecided(round, r uint64) (anchorDecision, bool) {
	impossible, byBlame := d.certImpossibility(r)
	if !impossible {
		return anchorDecision{kind: anchorWait}, false
	}

	if !byBlame && r == round+1 && !d.adjacentCertifyAgrees(round, r) {
		return anchorDecision{kind: anchorWait}, false
	}

	return anchorDecision{}, true
}

// adjacentCertifyAgrees reports whether certifying round r (== round+1) would resolve
// the queried round the same way a later anchor will. A later anchor two or more
// rounds above the queried one always carries its designated vertex by quorum
// intersection and COMMITS it; the adjacent round r is the only later round whose
// certified anchor sits one round up and could instead OMIT that vertex and SKIP.
// They agree exactly when every stored round-r vertex of r's designated producer
// cites the queried round's single designated vertex, so whichever one certifies also
// carries it. A missing or equivocated designated vertex on either side is read as
// disagreement, so the scan waits rather than pass on a reversible skip — UNLESS the
// queried round's producer is itself dead (no stored vertex and span-silent), the one
// missing-vertex case that is agreement rather than disagreement: with no candidate to
// carry, every resolution SKIPS the queried round, so r's certification cannot change
// the outcome.
func (d *DAG) adjacentCertifyAgrees(round, r uint64) bool {
	if d.deadDesignatedProducer(round) {
		// No candidate exists in any node's history: certifying r cannot carry a vertex
		// that is not there, and neither can a later anchor, so both paths skip. Silence
		// of the producer is what makes this agreement — a merely locally-absent producer
		// could still deliver a candidate a later certification would carry.
		return true
	}

	producer, ok := d.anchorProducerFor(round)
	if !ok {
		return false
	}

	candidates := d.designatedVertexSet(round, producer)
	if len(candidates) != 1 {
		return false // absent or equivocated: no single agreed outcome to prove
	}

	rVertices, ok := d.designatedRoundVertices(r)
	if !ok {
		return false // r's anchor vertex not held locally: cannot prove it carries the candidate
	}

	for _, rv := range rVertices {
		if !d.vertexCitesCandidate(rv, candidates) {
			return false
		}
	}

	return true
}

// designatedRoundVertices returns the round's designated producer's stored vertices.
// The bool is false when the round has no designated producer or the producer holds
// no vertex at the round.
func (d *DAG) designatedRoundVertices(round uint64) ([]Hash, bool) {
	producer, ok := d.anchorProducerFor(round)
	if !ok {
		return nil, false
	}

	return d.store.getByRoundProducer(round, producer)
}

// vertexCitesCandidate reports whether the stored vertex cites any candidate among
// its direct parents.
func (d *DAG) vertexCitesCandidate(hash Hash, candidates map[Hash]bool) bool {
	vertex := d.store.get(hash)
	if vertex == nil {
		return false
	}

	for _, parent := range appendParentHashes(nil, vertex) {
		if candidates[parent] {
			return true
		}
	}

	return false
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
// certified, so anchorStatus's forward scan may pass it. It is the disarming
// predicate for the permanent commit wedge: a run of undecided rounds (an anchor
// producer unreachable, or dead, when its round passed) otherwise blocks the scan
// from ever reaching the certified anchor that would resolve the wedged round. It
// collapses the two grades of impossibility its scan callers must distinguish into
// one bool; those callers use certImpossibility directly.
func (d *DAG) anchorCertImpossible(round uint64) bool {
	impossible, _ := d.certImpossibility(round)
	return impossible
}

// deadDesignatedProducer reports whether the round's designated producer stored NO
// vertex at the round AND is silent across the deep span above the frontier
// (anchorSilenceSpanRounds). Such a producer has, and will deliver, no candidate for
// any node to certify, so the round can never be certified — in EITHER regime, because
// certification needs a candidate to cite and there is none. The verdict rests on the
// silence timing assumption (a crashed producer is indistinguishable at the frontier
// from one delayed past the span), so callers treat the impossibility it establishes as
// REVERSIBLE and keep it under the same adjacent-round guard silence-impossibility
// already carries.
//
// Skipping such a round in the RELAXED bootstrap regime is no weaker a guarantee than
// the regime already offers: relaxed certification itself accepts a SINGLE supporter, so
// the regime concedes quorum intersection in its central rule, and a skip founded on the
// silence span sits within that same concession. The strict regime keeps the full
// intersection guard (adjacentCertifyAgrees), so its safety is unchanged. The result is
// false while the span is unobservable, the natural bootstrap guard silentHolders
// provides by returning an empty set on a shallow store.
func (d *DAG) deadDesignatedProducer(round uint64) bool {
	producer, ok := d.anchorProducerFor(round)
	if !ok {
		return false
	}

	set, ok := d.HoldersForEpoch(d.commitEpochForRound(round + 1))
	if !ok {
		return false
	}

	if len(d.designatedVertexSet(round, producer)) != 0 {
		return false // a stored candidate exists: this grade does not apply
	}

	return d.silentHolders(set, round+1)[producer]
}

// certImpossibility reports whether an undecided round can never certify and, when it
// cannot, whether IMMUTABLE blame alone establishes it (byBlame). The two grades
// differ in reversibility, which is why the caller needs them apart.
//
// A designated producer with NO stored vertex that is span-silent is ruled out FIRST,
// in ANY regime (deadDesignatedProducer): a round with no candidate cannot be certified
// under any rule, and the verdict is REVERSIBLE because it rests on the producer's
// silence. The stake-quorum grades below fire only in the STRICT regime — a relaxed
// bootstrap round certifies on a single supporter, so once it has a candidate it is
// never ruled out here — and only when the round-N+1 holder snapshot resolves. The rule
// is potential support: a holder is a BLAMER only when it has at least one STORED
// round-N+1 vertex and none of its stored round-N+1 vertices cites any of the
// candidate's vertices — the SAME union rule the direct verdict uses (tallyCitations),
// so a producer that ever cites a candidate, even via an equivocating second vertex, is
// a possible supporter, never a blamer. Every holder with NO stored round-N+1 vertex
// counts as a POTENTIAL SUPPORTER: its future vertex might cite the candidate. The round
// is impossible when even the maximum achievable support falls below the strict 2/3
// capped-stake quorum.
//
// The BLAME grade (byBlame true) rests only on stored blamer vertices, whose citations
// are parent hashes fixed at production. It is immutable and MONOTONE: more evidence
// only adds blamers, never removes them, so once a quorum's worth of blame is ruled
// out every node agrees forever. Passing a blame-impossible round can never fork.
//
// The SILENCE grade (byBlame false) additionally treats holders absent from a deep
// span above the frontier (anchorSilenceSpanRounds) as non-supporters, so a crashed
// validator's dead stake stops blocking the predicate. This grade is NOT monotone:
// the silent set SHRINKS as a slow holder's vertices arrive, so a starved node counts
// silence a better-informed node does not — the less-informed node is the readier to
// rule the round out. A holder absent because it crashed is indistinguishable, at the
// frontier, from one merely delayed past the span by a partition; the span's depth is
// a timing assumption, not a proof. The verdict a starved node reaches by silence can
// therefore be reversed on a node that holds the delayed vertices. Because of that,
// silence-impossibility is safe to ACT on only where a reversal cannot change an
// earlier round's resolution — the intersection guarantee two rounds up, enforced by
// scanPastUndecided's adjacent-round guard — never as a standalone license to skip.
func (d *DAG) certImpossibility(round uint64) (impossible, byBlame bool) {
	if d.deadDesignatedProducer(round) {
		return true, false // no candidate in any regime; reversible on the producer's silence
	}

	if d.roundIsRelaxed(round) {
		return false, false
	}

	producer, ok := d.anchorProducerFor(round)
	if !ok {
		return false, false
	}

	set, ok := d.HoldersForEpoch(d.commitEpochForRound(round + 1))
	if !ok {
		return false, false
	}

	tally := d.tallyCitations(round, producer)
	if d.certificationRuledOut(set, tally.blamers) {
		return true, true // immutable blame quorum
	}

	silent := d.silentHolders(set, round+1)
	if len(silent) == 0 {
		return false, false
	}

	if d.certificationRuledOut(set, mergeMemberSets(tally.blamers, silent)) {
		return true, false // reversible: rests on silence
	}

	return false, false
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
//
// Both skip paths are view-dependent in the RELAXED bootstrap regime, where a single
// supporter certifies: a node that decided before a straggler's backlog arrived would
// skip a round a better-informed peer commits. So in the relaxed regime the two skips
// become WAITs while the round is still resolvable — the candidate could still arrive,
// or a single supporter could still certify it — and settle only on evidence every
// node shares (the producer proven span-silent, or no possible supporter left). The
// STRICT regime keeps its immediate skips: its blame and silence guards fire upstream
// (scanPastUndecided), so a certified anchor here already resolves the round for good.
func (d *DAG) resolveIndirect(round uint64, certified Hash) anchorDecision {
	producer, ok := d.anchorProducerFor(round)
	if !ok {
		return anchorDecision{kind: anchorWait}
	}

	candidates, ok := d.store.getByRoundProducer(round, producer)
	if !ok {
		// Candidate absent locally. In the relaxed regime it may simply not have
		// arrived: skip only once the producer is provably span-silent, so a node that
		// decided before the backlog landed WAITs rather than skip a round a peer
		// certifies from the same producer's delivered vertex.
		if d.roundIsRelaxed(round) && !d.deadDesignatedProducer(round) {
			return anchorDecision{kind: anchorWait}
		}
		return anchorDecision{kind: anchorSkip}
	}

	history, ok := d.store.causalBatch(certified)
	if !ok {
		return anchorDecision{kind: anchorWait}
	}

	dec := decideMembership(candidates, history)
	if dec.kind == anchorSkip && d.roundIsRelaxed(round) && d.relaxedSupporterStillPossible(round, producer) {
		// The resolving anchor omits every candidate, but the relaxed regime certifies
		// on a single member supporter: while a holder that could still cite the
		// candidate remains (neither a stored blamer nor span-silent), the round may
		// yet certify directly, so WAIT rather than skip on a view that merely lacks
		// that holder's citation.
		return anchorDecision{kind: anchorWait}
	}

	return dec
}

// relaxedSupporterStillPossible reports whether an undecided relaxed round could still
// certify directly: a single holder that has not been seen to blame the candidate and
// is not span-silent could still produce a round-N+1 vertex citing it, and the relaxed
// regime needs exactly one member supporter to certify. While such a holder remains,
// resolving the round by a later anchor that omits the candidate would fork against a
// peer that receives that holder's citation. A holder is ruled out only by evidence
// every node shares: a stored round-N+1 vertex that cites no candidate (a blamer,
// fixed at production) or absence across the deep silence span. The caller holds
// commitMu (the commit path).
func (d *DAG) relaxedSupporterStillPossible(round uint64, producer Hash) bool {
	set, ok := d.HoldersForEpoch(d.commitEpochForRound(round + 1))
	if !ok {
		return false
	}

	blamers := d.tallyCitations(round, producer).blamers
	silent := d.silentHolders(set, round+1)

	for _, v := range set.All() {
		if !blamers[v.Pubkey] && !silent[v.Pubkey] {
			return true // a holder that could still cite the candidate remains
		}
	}

	return false
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
