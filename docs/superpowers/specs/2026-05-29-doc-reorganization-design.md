# Documentation reorganization design

Scope: how the project's documentation is structured. This covers which documents exist, what each one owns, and how content is split between them. It does not cover the technical content of the protocol, which lives in the whitepaper.

## Context

The project carries six markdown files, and three problems make them hard to trust.

The first is a duplicated design document. `bluepods_v2.md` (French, frozen in mid-February) and `WHITEPAPER.md` (English, current) describe the same architecture, and the French one has drifted out of date. Anyone reading the repository has to guess which one is authoritative, and the wrong guess is easy to make.

The second is a stale, untracked instruction file. `.claude/CLAUDE.md` describes an old layout (`BluePods-Node/`, `BluePods-Shared/`, `pods/BluePods-Interface/`) that no longer exists. The real top-level layout is `cmd/`, `internal/`, `client/`, `types/`, `wasm-gas/` (at the root, not under `pods/`), `pods/pod-sdk`, `pods/pod-system`, `test/`, and `deploy/`. The file also points at a `pods/ARCHITECTURE.md` that was never created. Because this is the file that orients an assistant, its inaccuracy is costly.

The third is that the vision is written nowhere. The reason the project exists (a decentralized cloud for hosting WASM backends), the properties it refuses to compromise (zero rollback, global atomic composability), and the tradeoff it accepts (atomicity and simplicity over horizontal scaling) lived only in the author's head until now. A reader, human or AI, cannot recover the "why" from any file.

## Goals and audience

The maintainer works solo. The documentation serves him returning after a break, and an AI agent that needs the project in context. There are no contributors and no plan for them, so the docs stay minimal: no contribution guide, and no status tracker to keep in sync.

All documents are in English.

## The two pillars

One rule holds the structure together: a given subject has exactly one document of record. The `bluepods_v2`/whitepaper duplication came from the opposite, two files telling the same story.

`VISION.md` answers why. It holds the reason the project exists, the non-negotiable properties (zero rollback, global atomic composability), the accepted tradeoff (atomicity and simplicity at the cost of horizontal scaling, the bandwidth bet), the non-goals, and the positioning against ICP, Sui, Solana, and Ethereum. It carries no technical detail.

`WHITEPAPER.md` answers how. It describes the target design (object model, consensus, attestation, execution sharding, transaction lifecycle, WASM execution, fees, validators, network, security), and keeps its design principles and its open problems. It keeps a one-paragraph statement of what the system is; the motivation and the standalone comparison move to VISION.

There is no separate status document. The maintainer works alone, and the open problems and known mismatches already live in the whitepaper's open-problems section and in the ATP. A status file would only be another thing to keep in sync.

## Boundary rules

Comparisons. VISION compares at the positioning level (ICP, Sui, Solana, Ethereum). The whitepaper keeps a technical comparison only where it justifies a specific design choice, written inline in the relevant section, not as a standalone comparison section.

Non-goals. VISION states each non-goal as a decision. The whitepaper does not re-explain it; where it must refer to one, it points to VISION.

The word "sharding". It has two meanings. Execution sharding (work routed to an object's holders) is a whitepaper mechanism. State or subnet sharding into independent chains is a VISION non-goal. Both documents use the qualified term so the two never read as a contradiction.

Whitepaper and code. The whitepaper describes the target system. Where it is wrong in absolute terms (a wrong constant, a mechanism described differently from how it is designed), fix it. Where the code is merely incomplete against the target, leave the whitepaper describing the target; that gap lives in the code and the ATP, not in a document that has to be maintained.

## Target structure

```
/  (repository root)
  README.md              build/run, plus the docs index (already present, updated)
  .claude/CLAUDE.md      updated: real layout, pointers to the pillars, writing conventions
  docs/
    VISION.md            new: why, cardinal properties, tradeoff, positioning
    WHITEPAPER.md        moved and updated: how (target design)
    ATP.md               moved: acceptance test plan
  test/integration/
    TESTING.md           stays in place, next to the test code it documents
```

`bluepods_v2.md` is deleted. Git keeps its history if it is ever needed.

## File-by-file transformation

`bluepods_v2.md` is removed as the obsolete duplicate. The whitepaper moves into `docs/`, drops its motivation and standalone comparison (now in VISION), keeps its design principles and open problems, and is realigned with the code under the bounded rule above. `docs/VISION.md` is created. `ATP.md` moves into `docs/` as the test plan.

The ATP names `bluepods_v2.md` as its source of truth on its fourth line. That line is redirected to the whitepaper and the code; otherwise deleting `bluepods_v2.md` leaves exactly the dead reference this work set out to remove.

`CLAUDE.md` is corrected so it points at the two pillars and describes the real layout, and its dead reference to `pods/ARCHITECTURE.md` is removed. It also carries the writing conventions. The file is intentionally outside git, so this update stays local, which is fine for a solo project.

The `README.md` already has a documentation index linking the whitepaper and the ATP. It is updated to add VISION and to fix the paths after the move into `docs/`.

`TESTING.md` stays in `test/integration/`, next to the test code it documents.

## Migration safety and order

Splitting the whitepaper is the only risky step. "Move" means relocate then reference, never delete without a destination: before any text is removed from the whitepaper, the same content must already exist in VISION, confirmed by a section-by-section pass.

The order: create VISION and fill it from the whitepaper's motivation and comparison; verify coverage; purify the whitepaper; move the whitepaper and ATP into `docs/` and re-route links (the ATP source-of-truth line and the README index); correct `CLAUDE.md`; delete `bluepods_v2.md` last.

## Writing conventions

The documents follow the humanizer guidance: direct prose, active voice, plain vocabulary, sentence-case headings, straight quotes, no em dashes, and bold reserved for where it earns its place. Explanation and reasoning are written as paragraphs. Structured data stays structured: a table of fee constants, a list of host functions, or the numbered steps of a protocol read more clearly as a table or list than as prose.

Voice is calibrated per pillar. VISION is a positioning document, so it carries opinion and takes sides. The whitepaper stays sober and factual.

## Keeping the docs in sync

The risk is recreating the drift. The single-document-per-subject rule and the boundary rules above keep two files from owning the same topic. Cross-references between the pillars live in the README index, and the last-updated date comes from git instead of being repeated in each file. No file is ever named "v2", "old", or "draft"; documents are edited in place and git holds the history.

## Out of scope

This work does not rewrite the protocol's technical content beyond aligning the whitepaper with the code under the bounded rule. It does not add a contribution guide, since there are no contributors. It does not change the test code, only the location decision for its documentation.
