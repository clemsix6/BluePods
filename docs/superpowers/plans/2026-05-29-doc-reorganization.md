# Documentation reorganization implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reorganize the project documentation into three pillars (VISION, WHITEPAPER, STATUS), delete the obsolete duplicate, and fix every stale reference, without losing content.

**Architecture:** Create and fill the two new pillars first, verify coverage, then purify the whitepaper, move files into `docs/`, re-route links, fix `CLAUDE.md`, and delete `bluepods_v2.md` last. This order guarantees no content is removed before its destination exists.

**Tech stack:** Markdown and git. No build, no code change. The repository is at `/Users/clement/BluePods` on branch `docs-reorganization`.

**Verification model:** This is a documentation migration, not code, so there are no unit tests. Each task ends with concrete verification (a `grep`, a file listing, or a content checklist) and a commit. Treat the verification step as the equivalent of a passing test: do not commit until it holds.

**Source of truth for splitting content:** the spec at `docs/superpowers/specs/2026-05-29-doc-reorganization-design.md`. Follow its boundary rules. Writing style follows the humanizer guidance recorded in the spec: prose for reasoning, structure for data, sentence-case headings, straight quotes, no em dashes.

**Commit convention (this repo):** `[+]` add, `[-]` remove, `[&]` change, `[!]` fix. One change per line; minor sub-points on their own `\t-` line. No footers, no co-author trailer.

---

## File structure

- Create `docs/VISION.md`: the why. Motivation, cardinal properties, accepted tradeoff, non-goals, positioning.
- Create `docs/STATUS.md`: the where. Done / in progress / to do, plus known spec-code gaps.
- Modify then move `WHITEPAPER.md` to `docs/WHITEPAPER.md`: the how. Purified of why and status content, realigned with code under the bounded rule.
- Move `ATP.md` to `docs/ATP.md`: acceptance test plan. Source-of-truth line redirected.
- Modify `.claude/CLAUDE.md` (untracked): real layout, pointers to the three pillars, writing conventions, STATUS-update rule.
- Modify `README.md`: update the existing docs index for the new files and the `docs/` paths.
- Delete `bluepods_v2.md`.
- `test/integration/TESTING.md`: unchanged, stays in place.

Content mapping from the current whitepaper (`WHITEPAPER.md`, section numbers as of today):

- Section 1 Introduction (Scalability Trilemma, A Different Approach), Section 2 Design Principles -> VISION (motivation and principles). The whitepaper keeps only a one-paragraph statement of what the system is.
- Section 14 Scaling Characteristics: the bandwidth bet and the atomicity-over-scaling tradeoff -> VISION. The factual latency and storage-cost analysis stays in the whitepaper.
- Section 15 Comparison with Existing Systems -> VISION as positioning (vs Sui, Solana, Ethereum). Add a new "vs ICP" entry; ICP is absent from the current docs and must be written fresh.
- Section 16 Open Problems -> STATUS (to do).
- Sections 3 through 13 and 17 stay in the whitepaper (the how), with Section 13 Security Analysis adjusted per the boundary rule (missing mechanisms described in the conditional, with a STATUS cross-reference).
- ATP Section 24 Security & Attack Vectors and Section 25 Spec-Code Mismatches -> seed STATUS.

---

## Task 1: Create docs/VISION.md

**Files:**
- Create: `docs/VISION.md`
- Read: `WHITEPAPER.md` (sections 1, 2, 14, 15)

- [ ] **Step 1: Write the file header and section skeleton**

Create `docs/VISION.md` starting with this header, then the section titles below it (sentence case, fill content in the next steps):

```markdown
# BluePods vision

Last updated: 2026-05-29
Scope: why this project exists, what it refuses to compromise, and how it positions against other systems. For how it works, see WHITEPAPER.md. For current state, see STATUS.md.

## What this is

## The problem

## Cardinal properties

## The tradeoff we accept

## Non-goals

## Positioning
```

- [ ] **Step 2: Fill "What this is" and "The problem"**

Write "What this is" in one short paragraph: a decentralized cloud for hosting arbitrary WASM backends (pods) on a subset of nodes rather than every node. Write "The problem" from the whitepaper's Section 1 (Scalability Trilemma in Practice, A Different Approach) and the principles of Section 2, as prose. State the motivation, not the mechanics.

- [ ] **Step 3: Fill "Cardinal properties" and "The tradeoff we accept"**

"Cardinal properties": zero rollback (deterministic absolute finality) and global synchronous atomic composability across all apps. State them as the lines the project will not cross. "The tradeoff we accept": a single global DAG and one total order, which buys those two properties at the cost of horizontal scaling, plus the bet that bandwidth keeps growing. Pull the bandwidth-bet and tradeoff framing from Section 14 (Bandwidth Scaling), leaving the factual latency and storage numbers for the whitepaper.

- [ ] **Step 4: Fill "Non-goals"**

List the non-goals as decisions with the why-not: no state or subnet sharding (it would make cross-shard finality conditional and break zero rollback), and no separate consensus-less fast path (it would break uniform global atomic composability). Use the qualified term "state/subnet sharding" so it never reads as a contradiction with the whitepaper's execution sharding.

- [ ] **Step 5: Fill "Positioning"**

Write the positioning at the system-class level, one short subsection each: vs Sui, vs Solana, vs Ethereum (adapt from Section 15), and a new vs ICP subsection (written fresh: ICP fragments into subnets so cross-subnet calls are async and non-atomic, which is exactly the property BluePods keeps with its global DAG). Keep it positioning, not technical justification.

- [ ] **Step 6: Verify the file is complete**

Run: `grep -c '^## ' docs/VISION.md`
Expected: `6` (six top-level sections, all present).
Run: `grep -ci 'em dash\|—' docs/VISION.md`
Expected: `0` (no em dashes).

- [ ] **Step 7: Commit**

```bash
git add docs/VISION.md
git commit -m "[+] Add VISION.md (project why and positioning)"
```

---

## Task 2: Create docs/STATUS.md

**Files:**
- Create: `docs/STATUS.md`
- Read: `WHITEPAPER.md` (section 16), `ATP.md` (sections 24, 25), and the code under `internal/`, `pods/`, `wasm-gas/` to confirm real state.

- [ ] **Step 1: Write the file header and section skeleton**

Create `docs/STATUS.md` with this header and skeleton:

```markdown
# BluePods status

Last updated: 2026-05-29
Scope: the gap between the design and the code. What works, what is partial, what is missing, and known spec-code mismatches. For the intended design, see WHITEPAPER.md.

## Done

## In progress

## To do

## Known spec-code gaps
```

- [ ] **Step 2: Fill "Done"**

List the subsystems that work, confirmed by reading the code, not by the old CLAUDE.md. Anchor list: DAG consensus, QUIC mesh networking, aggregated BLS attestation, rendezvous hashing, the WASM runtime, the system pod's functions. For each, one line. Where the old CLAUDE.md and the code disagree (it marks the system pod "in development" while the code implements its functions), the code wins.

- [ ] **Step 3: Fill "In progress" and "To do"**

"In progress": partial features confirmed from ATP Section 25 and the code, for example gas metering that is instrumented but only covers the entry block, and epoch rewards that are computed but not credited. "To do": the items from the whitepaper's Section 16 Open Problems (fraud proofs, storage challenges, aggregator failover, inactivity detection, slashing) plus runtime hardening. One line each, with a cross-reference to the relevant whitepaper section.

- [ ] **Step 4: Fill "Known spec-code gaps"**

Reproduce the seven items from ATP Section 25 as the seed, plus any relevant attack-vector gaps from ATP Section 24, each as a one-line entry with a pointer to the whitepaper section it contradicts. Confirm each against the code before listing it.

- [ ] **Step 5: Verify the file is complete**

Run: `grep -c '^## ' docs/STATUS.md`
Expected: `4`.
Run: `grep -ci 'TODO\|TBD\|fill in' docs/STATUS.md`
Expected: `0` (the "To do" heading is fine; this checks for leftover placeholders, so confirm any match is only the heading).

- [ ] **Step 6: Commit**

```bash
git add docs/STATUS.md
git commit -m "[+] Add STATUS.md (done / in progress / to do)"
```

---

## Task 3: Coverage gate before purifying the whitepaper

This task removes nothing. It confirms that everything scheduled to leave the whitepaper already lives in VISION or STATUS, so Task 4 cannot lose content.

**Files:**
- Read: `WHITEPAPER.md`, `docs/VISION.md`, `docs/STATUS.md`

- [ ] **Step 1: Check the motivation moved**

Confirm the Scalability Trilemma motivation, the "different approach" argument, and the design principles from whitepaper Sections 1 and 2 are present in `docs/VISION.md`. If any point is missing, add it to VISION before continuing.

- [ ] **Step 2: Check the comparison moved**

Confirm each comparison subsection from whitepaper Section 15 (vs Sui, vs Solana, vs Ethereum, What BluePods Borrows) is reflected in VISION's Positioning, and that the new vs ICP entry exists. If any is missing, add it.

- [ ] **Step 3: Check the open problems moved**

Confirm each item from whitepaper Section 16 Open Problems appears in STATUS under "To do" or "Known spec-code gaps". If any is missing, add it.

- [ ] **Step 4: Record the gate result**

Append a one-line note to the bottom of the plan file's Task 3 (or your working notes) listing any content you had to add during this gate. If nothing was missing, state that. This is the explicit confirmation that purge is safe.

- [ ] **Step 5: Commit any coverage fixes**

```bash
git add docs/VISION.md docs/STATUS.md
git commit -m "[&] Backfill VISION/STATUS coverage before whitepaper purge"
```

If nothing changed, skip the commit.

---

## Task 4: Purify and update WHITEPAPER.md in place

The whitepaper is still at the repository root for this task; it moves in Task 5.

**Files:**
- Modify: `WHITEPAPER.md`

- [ ] **Step 1: Add the header**

Add at the top of `WHITEPAPER.md`, under the title:

```markdown
Last updated: 2026-05-29
Scope: how the system works (the target design). For why it exists and how it compares to others, see VISION.md. For what is actually implemented, see STATUS.md.
```

- [ ] **Step 2: Remove the migrated sections**

Delete Section 1 (Introduction) down to a single one-paragraph statement of what the system is, delete Section 2 (Design Principles), delete Section 15 (Comparison with Existing Systems), and delete Section 16 (Open Problems). From Section 14, remove the bandwidth-bet and tradeoff framing, keeping the factual latency and storage-cost analysis. Renumber remaining sections.

- [ ] **Step 3: Disambiguate "sharding"**

In Section 5 (Storage Distribution) and Section 8 (Transaction Lifecycle / Execution), replace bare "sharding" with "execution sharding" wherever it means work routed to holders, so it never collides with VISION's "state/subnet sharding" non-goal.

- [ ] **Step 4: Apply the bounded realignment**

Where the whitepaper states something wrong in absolute terms (a wrong constant, a mechanism described differently from how it is designed), fix it. Do not lower the target to the current partial implementation; those gaps belong in STATUS. In Section 13 (Security Analysis), describe any mechanism that does not exist in the code in the conditional, with a cross-reference to STATUS.

- [ ] **Step 5: Verify the purge**

Run: `grep -c 'vs. Sui\|Open Problems\|Design Principles' WHITEPAPER.md`
Expected: `0`.
Run: `grep -c 'execution sharding' WHITEPAPER.md`
Expected: `1` or more.
Run: `grep -c 'Last updated:' WHITEPAPER.md`
Expected: `1`.

- [ ] **Step 6: Commit**

```bash
git add WHITEPAPER.md
git commit -m "$(printf '[&] Purify whitepaper to design-only content\n\t- Move why/principles to VISION, open problems to STATUS\n\t- Disambiguate execution sharding, add scope header\n\t- Security: missing mechanisms in conditional with STATUS xref')"
```

---

## Task 5: Move whitepaper and ATP into docs/ and re-route links

**Files:**
- Move: `WHITEPAPER.md` -> `docs/WHITEPAPER.md`
- Move: `ATP.md` -> `docs/ATP.md`
- Modify: `docs/ATP.md` (source-of-truth line)
- Modify: `README.md` (index and paths)

- [ ] **Step 1: Move the files with history preserved**

```bash
git mv WHITEPAPER.md docs/WHITEPAPER.md
git mv ATP.md docs/ATP.md
```

- [ ] **Step 2: Redirect the ATP source-of-truth line**

In `docs/ATP.md`, replace line 4 `Source of truth: the code and `bluepods_v2.md`.` with `Source of truth: the code and docs/WHITEPAPER.md.`

- [ ] **Step 3: Update the README index and paths**

In `README.md`, update the existing Documentation section (around lines 118 and 124-128): change the `WHITEPAPER.md` link to `docs/WHITEPAPER.md`, the `ATP.md` links to `docs/ATP.md`, add a link to `docs/VISION.md` (the why) and `docs/STATUS.md` (current state), and keep the `test/integration/TESTING.md` link as is.

- [ ] **Step 4: Verify links and locations**

Run: `ls docs/`
Expected: `ATP.md  STATUS.md  VISION.md  WHITEPAPER.md  superpowers`
Run: `grep -rn 'bluepods_v2' docs/ATP.md README.md`
Expected: no matches.
Run: `grep -c 'docs/VISION.md\|docs/STATUS.md\|docs/WHITEPAPER.md\|docs/ATP.md' README.md`
Expected: `4` or more.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "$(printf '[&] Move whitepaper and ATP into docs/, re-route links\n\t- git mv WHITEPAPER.md and ATP.md to docs/\n\t- Redirect ATP source-of-truth to whitepaper and code\n\t- Update README index with VISION, STATUS and docs/ paths')"
```

---

## Task 6: Fix .claude/CLAUDE.md

This file is untracked (in `.gitignore`), so it will not appear in commits. Edit it anyway; the update stays local, which is accepted.

**Files:**
- Modify: `.claude/CLAUDE.md`

- [ ] **Step 1: Replace the context-files section**

Replace the lines that tell the reader to read `bluepods_v2.md` and `pods/ARCHITECTURE.md` with pointers to the three pillars: read `docs/VISION.md` for the why, `docs/WHITEPAPER.md` for the architecture, and `docs/STATUS.md` for current state. Remove the dead `pods/ARCHITECTURE.md` reference entirely.

- [ ] **Step 2: Fix the project layout**

Replace the old layout description (`BluePods-Node/`, `BluePods-Shared/`, `pods/BluePods-Interface/`) with the real one: `cmd/`, `internal/`, `client/`, `types/`, `wasm-gas/` (at the root, not under `pods/`), `pods/pod-sdk`, `pods/pod-system`, `test/`, `deploy/`.

- [ ] **Step 3: Add the writing conventions and STATUS-update rule**

Add a short section stating the writing conventions (prose for reasoning, structure for data, sentence-case headings, no em dashes, straight quotes) and the STATUS-update rule with its objective trigger: any commit that changes the behavior of a subsystem tracked in `docs/STATUS.md` must touch STATUS in the same commit. Note that a pre-commit or CI check is a possible later addition, not built now.

- [ ] **Step 4: Verify**

Run: `grep -c 'ARCHITECTURE.md\|BluePods-Node\|bluepods_v2' .claude/CLAUDE.md`
Expected: `0`.
Run: `grep -c 'docs/VISION.md\|docs/WHITEPAPER.md\|docs/STATUS.md' .claude/CLAUDE.md`
Expected: `3` or more.

- [ ] **Step 5: No commit**

`.claude/CLAUDE.md` is untracked, so there is nothing to commit. Confirm with `git status --short .claude/` showing no entry.

---

## Task 7: Delete bluepods_v2.md (last)

Only after every reference is re-routed and the new pillars are in place.

**Files:**
- Delete: `bluepods_v2.md`

- [ ] **Step 1: Confirm no live references remain**

Run: `grep -rln 'bluepods_v2' . --include='*.md' | grep -v 'docs/superpowers/'`
Expected: no matches. (The spec and this plan under `docs/superpowers/` mention it by design; everything else must be clean before deletion.)

- [ ] **Step 2: Delete the file**

```bash
git rm bluepods_v2.md
```

- [ ] **Step 3: Verify the working tree**

Run: `git ls-files | grep -c 'bluepods_v2'`
Expected: `0`.
Run: `ls docs/`
Expected: the four pillars (`VISION.md`, `WHITEPAPER.md`, `STATUS.md`, `ATP.md`) plus `superpowers`.

- [ ] **Step 4: Commit**

```bash
git commit -m "[-] Delete obsolete bluepods_v2.md (superseded by docs/ pillars)"
```

---

## Final verification

- [ ] Run: `git log --oneline docs-reorganization` and confirm the sequence of commits matches the seven tasks.
- [ ] Run: `grep -rln 'bluepods_v2\|pods/ARCHITECTURE.md\|BluePods-Node' . --include='*.md' | grep -v 'docs/superpowers/'` and confirm no matches.
- [ ] Open `docs/VISION.md`, `docs/WHITEPAPER.md`, `docs/STATUS.md` and confirm each has the `Last updated` header and a scope line, and that no topic is covered in two pillars.
