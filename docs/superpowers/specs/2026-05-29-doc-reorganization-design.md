# Documentation reorganization design

Last updated: 2026-05-29
Scope: how the project's documentation is structured. This covers which documents exist, what each one owns, and how they stay accurate. It does not cover the technical content of the protocol, which lives in the whitepaper.

## Context

The project carries six markdown files, and three problems make them hard to trust.

The first is a duplicated design document. `bluepods_v2.md` (French, frozen in mid-February) and `WHITEPAPER.md` (English, current) describe the same architecture, and the French one has drifted out of date. Anyone reading the repository has to guess which one is authoritative, and the wrong guess is easy to make.

The second is a stale, untracked instruction file. `.claude/CLAUDE.md` describes an old layout (`BluePods-Node/`, `BluePods-Shared/`, `pods/BluePods-Interface/`) that no longer exists. The real layout is `internal/`, `pods/pod-sdk`, and `pods/pod-system`. The file also points at a `pods/ARCHITECTURE.md` that was never created. Because this is the file that orients an assistant, its inaccuracy is costly.

The third is that the vision is written nowhere. The reason the project exists (a decentralized cloud for hosting WASM backends), the properties it refuses to compromise (zero rollback, global atomic composability), and the tradeoff it accepts (atomicity and simplicity over horizontal scaling) lived only in the author's head until now. A reader, human or AI, cannot recover the "why" from any file.

## Goals and audience

The documentation serves three readers: the maintainer returning after a break, an AI agent that needs the full project in context, and an external reader who wants to understand the project. There are no contributors yet, so the documentation does not need a contribution guide or an onboarding flow.

All documents are written in English, which matches the existing whitepaper and the project's own convention.

## The three pillars

One rule holds the structure together: a given subject has exactly one document of record. The `bluepods_v2`/whitepaper duplication came from the opposite, two files telling the same story. Each pillar owns one question.

`VISION.md` answers why. It holds the project's reason for existing (decentralized cloud, WASM backends hosted on a subset of nodes), the non-negotiable properties (zero rollback, global atomic composability), the accepted tradeoff (atomicity and simplicity at the cost of horizontal scaling, the bet on growing bandwidth), the explicit non-goals (no sharding or subnets, no separate consensus-less fast path), and the positioning against ICP, Sui, and Solana. It never drops into technical detail or implementation status.

`WHITEPAPER.md` answers how. It keeps its current structure (object model, DAG consensus, BLS attestation, sharding, transaction lifecycle, WASM execution, fees, security) and describes the ideal system. It is purified of what does not belong: the rationale and comparisons move to VISION, the open problems move to STATUS. It is also realigned with the actual code where the two have drifted.

`STATUS.md` answers where things stand. It is a living retrospective in three parts. Done covers what works: DAG consensus, the QUIC mesh, aggregated BLS, rendezvous hashing, the WASM runtime, the system pod. In progress covers what is partial: gas metering that is instrumented but limited to the entry block, epoch rewards that are computed but never credited. To do covers what remains: fraud proofs, slashing, storage challenges, runtime hardening, and the known fixes. The spec-code mismatches from ATP section 25 and the open problems from the whitepaper move here, since they had no logical home before.

## Target structure

```
/  (repository root)
  README.md              build/run, plus an index of the docs
  .claude/CLAUDE.md      updated: real layout and pointers to the pillars
  docs/
    VISION.md            new: why, cardinal properties, tradeoff, positioning
    WHITEPAPER.md        moved and updated: how (theory)
    STATUS.md            new: done / in progress / to do, spec-code gaps
    ATP.md               moved: acceptance test plan
  test/integration/
    TESTING.md           stays in place, next to the test code it documents
```

`bluepods_v2.md` is deleted. Git keeps its history if it is ever needed.

## File-by-file transformation

`bluepods_v2.md` is removed as the obsolete duplicate. The whitepaper moves into `docs/`, is purified, and is realigned with the code. `docs/VISION.md` and `docs/STATUS.md` are created. `ATP.md` moves into `docs/` as the test plan, with its section 25 migrated into STATUS.

Two files need a specific note. `CLAUDE.md` is corrected so it points at the three pillars and describes the real layout, and its dead reference to `pods/ARCHITECTURE.md` is removed. Because the file is intentionally outside git, this update stays local, which is fine without contributors. `README.md` gains an index of the documents.

`TESTING.md` stays in `test/integration/`. Its value comes from sitting next to the test code it describes, and it answers a tooling question rather than a design question, so it belongs in a different register from the three pillars.

## Writing conventions

The documents follow the humanizer guidance: direct prose, active voice, plain vocabulary, sentence-case headings, straight quotes, no em dashes, and bold reserved for where it earns its place. Explanation and reasoning are written as paragraphs. Structured data stays structured: a table of fee constants, a list of host functions, or the numbered steps of a protocol read more clearly as a table or list than as prose, and forcing them into paragraphs would hurt the document.

Voice is calibrated per pillar. VISION is a positioning document, so it carries opinion and takes sides. WHITEPAPER and STATUS stay sober and factual, without forced personality.

## Keeping the docs in sync

The real risk is recreating the drift in six months. Three habits prevent it. The single-document-per-subject rule, stated above, keeps two files from ever owning the same topic. A header on each file gives its last-updated date and a one-line scope: what it covers, what it does not, and where to look for the rest. A rule written into `CLAUDE.md` requires STATUS to be updated at the end of every serious work session, since it is the document that goes stale fastest. No file is ever named "v2", "old", or "draft"; documents are edited in place and git holds the history.

## Out of scope

This work does not rewrite the protocol's technical content beyond aligning the whitepaper with the code. It does not add a contribution guide, since there are no contributors yet. It does not change the test code itself, only the location decision for its documentation.
