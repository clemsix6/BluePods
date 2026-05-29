# Documentation reorganization design

Last updated: 2026-05-29
Scope: how the project's documentation is structured. This covers which documents exist, what each one owns, how content is split between them, and how they stay accurate. It does not cover the technical content of the protocol, which lives in the whitepaper.

## Context

The project carries six markdown files, and three problems make them hard to trust.

The first is a duplicated design document. `bluepods_v2.md` (French, frozen in mid-February) and `WHITEPAPER.md` (English, current) describe the same architecture, and the French one has drifted out of date. Anyone reading the repository has to guess which one is authoritative, and the wrong guess is easy to make.

The second is a stale, untracked instruction file. `.claude/CLAUDE.md` describes an old layout (`BluePods-Node/`, `BluePods-Shared/`, `pods/BluePods-Interface/`) that no longer exists. The real top-level layout is `cmd/`, `internal/`, `client/`, `types/`, `wasm-gas/` (at the root, not under `pods/`), `pods/pod-sdk`, `pods/pod-system`, `test/`, and `deploy/`. The file also points at a `pods/ARCHITECTURE.md` that was never created. Because this is the file that orients an assistant, its inaccuracy is costly.

The third is that the vision is written nowhere. The reason the project exists (a decentralized cloud for hosting WASM backends), the properties it refuses to compromise (zero rollback, global atomic composability), and the tradeoff it accepts (atomicity and simplicity over horizontal scaling) lived only in the author's head until now. A reader, human or AI, cannot recover the "why" from any file.

## Goals and audience

The documentation serves three readers: the maintainer returning after a break, an AI agent that needs the full project in context, and an external reader who wants to understand the project. There are no contributors yet, so the documentation does not need a contribution guide or an onboarding flow.

All documents are written in English, which matches the existing whitepaper and the project's own convention.

## The three pillars

One rule holds the structure together: a given subject has exactly one document of record. The `bluepods_v2`/whitepaper duplication came from the opposite, two files telling the same story. Each pillar owns one question.

`VISION.md` answers why. It holds the project's reason for existing (decentralized cloud, WASM backends hosted on a subset of nodes), the non-negotiable properties (zero rollback, global atomic composability), the accepted tradeoff (atomicity and simplicity at the cost of horizontal scaling, the bet on growing bandwidth), the explicit non-goals (no state or subnet sharding, no separate consensus-less fast path), and the positioning against ICP, Sui, Solana, and Ethereum. It never drops into technical detail or implementation status.

`WHITEPAPER.md` answers how. It keeps its current structure (object model, DAG consensus, BLS attestation, execution sharding, transaction lifecycle, WASM execution, fees, security) and describes the target system. It is purified of what does not belong: positioning and standalone comparisons move to VISION, open problems move to STATUS. It is also realigned with the code under the bounded rule stated below.

`STATUS.md` answers where things stand. It is a living retrospective in three parts. Done covers what works: DAG consensus, the QUIC mesh, aggregated BLS, rendezvous hashing, the WASM runtime, the system pod. In progress covers what is partial: gas metering instrumented but limited to the entry block, epoch rewards computed but not credited. To do covers what remains: fraud proofs, slashing, storage challenges, runtime hardening, and the known fixes. STATUS is derived from the actual code plus three documented sources that have no other logical home: the whitepaper's open problems, the ATP's attack vectors (section 24), and the ATP's spec-code mismatches (section 25). Where the old CLAUDE.md and the code disagree (for example, the system pod marked "in development" there but actually implemented), the code wins and STATUS records the real state.

## Boundary rules between pillars

The duplication that started this work came from leaving boundaries implicit. These rules make the split decidable so the next editor never has to guess.

Security. The whitepaper describes the security model as it should hold: assumptions, intended mechanisms, target properties. STATUS records the gap with the real code: missing mechanisms, assumptions not yet enforced, known bugs. When a mechanism does not exist in the code, the whitepaper describes it in the conditional and STATUS lists it as missing, with a cross-reference. A single fact like "fraud proofs and slashing are not implemented" lives as a target in the whitepaper and as a to-do in STATUS, linked, not duplicated freely.

Comparisons. VISION compares at the positioning level: what class of system this is and what it refuses to do, covering ICP, Sui, Solana, and Ethereum. The whitepaper keeps a technical comparison only where it justifies a specific design choice, written as a sentence inside the relevant section, never as a standalone comparison section. The existing Sui/Solana/Ethereum comparison moves to VISION at the positioning level; the ICP comparison is new material to write, since it is absent from the current docs.

Non-goals. VISION states each non-goal as a decision: the what and the why-not. The whitepaper does not re-explain a non-goal; where it must refer to one, it points to VISION.

The word "sharding". It has two meanings in these docs. Execution sharding (work routed to an object's holders) is something the system does, and the whitepaper describes it. State or subnet sharding into independent chains is a non-goal stated in VISION. Both documents use the qualified term ("execution sharding", "state/subnet sharding") so the two never read as a contradiction.

Whitepaper realignment. The whitepaper always describes the target system. "Realign with the code" means correcting only what is wrong in absolute terms, such as a wrong constant or a mechanism described differently from how it is designed. It does not mean lowering the target to the level of a partial implementation. Gaps between the target and the current code go to STATUS. For example, the whitepaper describes complete gas metering as the design, while the fact that the current instrumentation only covers the entry block is a STATUS entry, not a whitepaper edit.

## Target structure

```
/  (repository root)
  README.md              build/run, plus the docs index (already present, updated)
  .claude/CLAUDE.md      updated: real layout, pointers to the pillars,
                         writing conventions, and the STATUS-update rule
  docs/
    VISION.md            new: why, cardinal properties, tradeoff, positioning
    WHITEPAPER.md        moved and updated: how (target system)
    STATUS.md            new: done / in progress / to do, spec-code gaps
    ATP.md               moved: acceptance test plan
  test/integration/
    TESTING.md           stays in place, next to the test code it documents
```

`bluepods_v2.md` is deleted. Git keeps its history if it is ever needed.

## File-by-file transformation

`bluepods_v2.md` is removed as the obsolete duplicate. The whitepaper moves into `docs/`, is purified, and is realigned with the code under the bounded rule above. `docs/VISION.md` and `docs/STATUS.md` are created. `ATP.md` moves into `docs/` as the test plan.

The ATP names `bluepods_v2.md` as its source of truth on its fourth line. That line is redirected to the whitepaper and the code; otherwise deleting `bluepods_v2.md` leaves exactly the dead reference this work set out to remove. Its sections 24 and 25 feed STATUS.

`CLAUDE.md` is corrected so it points at the three pillars and describes the real layout, and its dead reference to `pods/ARCHITECTURE.md` is removed. It also carries the writing conventions and the STATUS-update rule. The file is intentionally outside git, so those rules stay local and are best-effort. That is an accepted tradeoff for a solo project with no contributors.

The `README.md` already has a documentation index linking the whitepaper, the ATP, and TESTING.md. It is updated to add VISION and STATUS and to fix the paths after the move into `docs/`.

`TESTING.md` stays in `test/integration/`. Its value comes from sitting next to the test code it describes, and it answers a tooling question rather than a design question, so it belongs in a different register from the three pillars.

## Migration safety and order

Splitting a single 48 KB whitepaper into three files is the riskiest step, so the plan protects against losing content. "Move" means relocate then reference, never delete without a destination. Before any text is removed from the whitepaper, the same content must already exist in VISION or STATUS, confirmed by a section-by-section pass.

The order of operations follows from that. First, create VISION and STATUS and fill them from the whitepaper's positioning, comparisons, and open problems, plus the ATP's sections 24 and 25 and a read of the code. Second, verify coverage section by section. Third, purify the whitepaper. Fourth, move the whitepaper and the ATP into `docs/`, then grep for internal links and paths and re-route them, including the ATP source-of-truth line and the README index. Fifth, correct `CLAUDE.md`. Delete `bluepods_v2.md` last, once everything else is in place.

## Writing conventions

The documents follow the humanizer guidance: direct prose, active voice, plain vocabulary, sentence-case headings, straight quotes, no em dashes, and bold reserved for where it earns its place. Explanation and reasoning are written as paragraphs. Structured data stays structured: a table of fee constants, a list of host functions, or the numbered steps of a protocol read more clearly as a table or list than as prose, and forcing them into paragraphs would hurt the document.

Voice is calibrated per pillar. VISION is a positioning document, so it carries opinion and takes sides. WHITEPAPER and STATUS stay sober and factual, without forced personality.

## Keeping the docs in sync

The real risk is recreating the drift in six months. Three habits prevent it. The single-document-per-subject rule and the boundary rules above keep two files from ever owning the same topic. A header on each pillar gives its last-updated date and a one-line scope: what it covers, what it does not, and where to look for the rest. The writing conventions and a STATUS-update rule live in `CLAUDE.md`, with an objective trigger: any commit that changes the behavior of a subsystem tracked in STATUS must touch STATUS. Because `CLAUDE.md` is outside git, this rule is local and best-effort, which is accepted here; a pre-commit or CI check that warns when `internal/` changes without STATUS moving is noted as a possible later addition, not built now. No file is ever named "v2", "old", or "draft"; documents are edited in place and git holds the history.

## Out of scope

This work does not rewrite the protocol's technical content beyond aligning the whitepaper with the code under the bounded rule. It does not add a contribution guide, since there are no contributors yet. It does not change the test code itself, only the location decision for its documentation. It does not build the pre-commit or CI check; that is left as a future option.
