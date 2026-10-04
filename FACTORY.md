# Factory

A three-seat software factory in Band Desktop. A lead plans, an implementer builds, and an
independent reviewer checks the work against the specification before anything is accepted.
The human sends one task per stage and nothing else.

The mandates in [`mandates/`](mandates/) describe how each seat works and name nothing about
the problem, so the same band can be pointed at a different specification. Step-by-step setup
is in [`SETUP.md`](SETUP.md); the exact task sent for each stage is in [`dispatch/`](dispatch/).

## Seats

| Seat | Owns | Harness | Model | Paid from |
|---|---|---|---|---|
| **Architect** | Stage folder setup, `DESIGN.md`, handoffs, routing review results, final report | OpenCode (via `agents/architect.py`) | `MiniMaxAI/MiniMax-M2.5` on Featherless | Featherless credit |
| **Developer** | Application source, unit tests, `Dockerfile`, `RUN.md` | Claude Code (Band Desktop local agent) | `claude-sonnet-5-5`, medium effort | Claude Pro plan |
| **QA Lead** | Acceptance tests written from the spec, clean-container build, harness runs, PASS/REJECT verdict | OpenCode (via `agents/qa_lead.py`) | `MiniMaxAI/MiniMax-M2.5` on Featherless | Featherless credit |

All three work in one shared result repository. Each commits only its own files, as its own
Git author, so the history shows who did what.

### How work flows through a stage

1. The human sends the task to **Architect**. That message is the only human input.
2. **Architect** copies the previous stage folder forward (or creates the first one), commits
   the copy, writes and commits a short `DESIGN.md`, and hands **Developer** the complete task
   and specification text. A pointer to an earlier message is never a handoff.
3. **Developer** implements, builds the image, runs its tests, commits, and reports the full
   revision hash to **QA Lead** and **Architect**.
4. **Architect** hands **QA Lead** the same complete requirements plus that revision.
5. **QA Lead** checks out the revision, writes acceptance tests from the specification,
   builds from a clean checkout by following `RUN.md`, runs the evaluation command in normal
   and isolated mode, and replies **PASS** or **REJECT** with each failure, the requirement it
   breaks, and the exact command and output.
6. On REJECT, **Architect** routes the failures back to **Developer** and the loop repeats.
   After three rejected rounds on the same problem, Architect re-plans that work item.
7. On PASS, **Architect** posts the final report: stage, accepted revision, what passed,
   known gaps.

## Design choices and what they cost

| Choice | Why | Cost |
|---|---|---|
| **Hybrid models**: Claude only for the implementer, open-weights models for planning and review | We had a Claude Pro plan and a $25 Featherless credit. The implementer writes the graded code and uses the most tokens, so it gets the strongest model; planning and review use a cheaper one | Two runtimes to set up (Band-hosted Claude Code plus an OpenCode adapter in WSL) |
| **Reviewer on a different model family from the implementer** | An independent check is worth more when the checker does not share the builder's blind spots | MiniMax writes long outputs slowly (about 26 tokens/s), so review turns take minutes |
| **Full specification pasted into every handoff** | Seats see only messages addressed to them; a seat that misses a requirement cannot recover it | Long handoffs cost output tokens and time on the slow model |
| **Short `DESIGN.md`**: decisions and work items only, never a restatement of the spec | The first version tried to restate the spec and blew the turn time limit (below) | Less detail in the design; the spec in the handoff carries it instead |
| **One seat works at a time** | Simple to follow in the room log; avoids two seats editing the shared tree at once | No parallelism |
| **Stage folders copied forward and earlier folders never edited** | Each folder must still pass every earlier suite; the history shows requirements accumulating | Duplicate code across folders, by design |
| **Developer at medium reasoning effort** | Stalling on the Pro usage limit mid-stage would end the run, since nobody may nudge it | Possibly lower code quality than high effort; QA's loop compensates |

## What we tried that failed

Everything below was found in practice runs before the submitted run, or in aborted starts
that were discarded. Each fix is in the mandates or the seat runner.

| What happened | Fix |
|---|---|
| **DeepSeek-V4-Flash dropped arguments from multi-parameter tool calls.** Posting a message needs room id, text and mentions; the model sent one or none, Band rejected it, and the reviewer retried about 15 times | Switched both OpenCode seats to MiniMax-M2.5, which the event organisers had tested with OpenCode |
| **Seats replied to each other's replies without end.** Every tagged message triggered another acknowledgement | Mandate rule: reply only when a message asks you to act or answer, never to an acknowledgement, and tag a seat only when you need it to act |
| **Short `@architect` handles might not resolve.** Band agents are addressed as `@<owner>/<agent>` | Mandates use the full handles |
| **QA's work was committed under the Architect's name.** QA left its tests uncommitted; the Architect staged everything and swept them up | Mandate rule: stage only your own files, by path; author every commit as your seat; commit before handing off |
| **The Architect's first turn hit the 15-minute adapter timeout** while writing a long `DESIGN.md` from a 20,000-character specification | Raised the turn timeout to 60 minutes in `agents/seat.py`, and told the Architect to keep `DESIGN.md` short |
| **QA's first isolated-mode run exceeded its 5-minute shell timeout**, because the first isolated run builds the test-runner image | Later dispatches tell the reviewer to give that command at least 20 minutes. Reruns use the cached image and take about 80 seconds |
| **A seat started in the practice repository** after the Band runtime's working directory was changed, because old room sessions keep their original directory | Verified each seat's working directory before the submitted run; a new room starts a fresh session |

## How the factory catches bad work

- **Independent verification.** The implementer never accepts its own work. QA checks out the
  exact reported revision, builds it from a clean checkout and runs the checks itself.
- **Acceptance tests from the specification, not from the shipped checks.** QA lists every
  requirement, with attention to what the sample checks never exercise: repeated and
  concurrent requests, invalid input, boundaries, and export/import round trips.
- **Graded-mode check before PASS.** QA runs the evaluation in isolated mode (no network,
  2 CPUs, 2 GiB) because that is how the service is graded.
- **Earlier stages protected.** Each folder is checked against every earlier suite, and
  accepted folders are never edited. We confirmed with `git diff` that `stage-1/`,
  `stage-2/` and `stage-3/` were unchanged after later stages.

**Caught in the submitted run, stage 1:** QA ran the shipped checks against the first
implementation and found 10 of 120 failing (01:05). It rejected the revision with three root
causes: a wrong-typed signup field returned 422 instead of 400 `malformed_request`; table ids
were not scoped per restaurant, so a fixture reusing an id across restaurants hit a unique
constraint and failed six time-zone tests; and reset silently accepted seeded references
outside 6–12 characters of `A-Z0-9`. Architect routed the failures to Developer, who fixed
all three in one commit (01:07); QA re-ran the checks and passed the new revision (01:12).

**Caught in the submitted run, stage 3, with every shipped check green:** QA first reported
PASS on the harness results (02:11), then kept reading the specification and posted FAIL at
02:14: "Harness: PASS … Spec Deviations Found". Series creation accepted requests without an
idempotency key, and fetching a series without authentication returned the wrong status.
Neither case is covered by the shipped checks. Architect routed both to Developer, who fixed
them by 02:17, and QA passed the new revision at 02:20. This is the reviewer doing what the
shipped checks cannot: testing the specification rather than the samples.

## Results of the submitted run

One room, one result repository, one dispatch per stage, no other human input.

Times are from `room.json`: the dispatch, then the Architect's final report.

| Stage | Time (MYT, Mon Oct 5) | Outcome | Isolated-mode check by the operator |
|---|---|---|---|
| 1 | 00:39 → 01:18 | PASS after QA rejected 10 failing shipped checks | `claimed stage: 1` |
| 2 | 01:27 → 01:50 | PASS | `claimed stage: 2`, including the upgrade from stage 1 |
| 3 | 01:55 → 02:23 | PASS after QA rejected two spec deviations the shipped checks missed | `claimed stage: 3`, including upgrades from stages 1–2 |
| 4 | 03:01 → 03:15 | PASS | `claimed stage: 4`, including upgrades from stages 1–3 |

After each final report the operator ran the event harness in isolated mode, the graded
configuration, without posting anything to the room. A final `harness run --all --mode isolated`
over all four folders reported every folder claiming its own stage. The shipped checks are
only part of the graded tests, so these are directional results.

## Known gaps

What the room log and history show that the factory did not do well:

- **QA twice posted PASS before finishing its checks.** In stage 1 (01:03) and stage 3
  (02:11) it reported PASS on its first checks, and Architect forwarded an early report. QA then
  completed its review and rejected the revision minutes later. The rejection stood and the
  fix went through the loop, but the early reports are noise in the log.
- **QA committed acceptance tests only in stage 1.** In stages 2–4 it verified with the
  harness and by reading the specification, but did not add new acceptance tests, so its later
  work appears in the room and not in the Git history.
- **QA's stage-1 isolated-mode run timed out** on its 5-minute shell limit (first build of the
  test-runner image). The Architect's stage-1 report says so. The operator confirmed isolated
  mode with the harness afterwards; later dispatches gave the command a 20-minute limit.
- **Developer wrote all application code.** That is the design: Architect plans and
  coordinates, QA verifies. Work is shared by role, not by splitting the code.

## Measured cost and time

| Run | Featherless credit | Claude Pro usage | Wall time |
|---|---|---|---|
| Toy stage 1 rehearsal | $0.11 | 3% of a 5-hour window | 23 min |
| Stage 1 (credit includes two discarded starts) | $0.40 | 8% | 39 min |
| Stage 2 | $0.12 | 12% | 23 min |
| Stage 3 | $0.23 | 16% | 28 min |
| Stage 4 | $0.13 | 9% (fresh window) | 14 min |
| **Submitted run, stages 1–4** | **$0.88** | **45%** of a 5-hour window in total (one reset before stage 4) | **1 h 44 min** (sum of the four stages) |
| **Everything including the toy rehearsal** | **$0.99 of $25** | | |

Band's agent card estimated the Developer's practice usage at about $0.43 at API prices; on
the Pro plan it was not billed separately. The Claude percentages also include the operator's
own Claude Code session running alongside, so they overstate the Developer's share.

## Standing it up

1. Follow [`SETUP.md`](SETUP.md): WSL with Docker, the event harness, OpenCode with a
   Featherless provider, three Band seats, and the two seat scripts.
2. Load each mandate as that seat's instructions. For the OpenCode seats the runner does this
   from `mandates/<seat>.md`; for the Band-hosted Developer, choose the file as its Role.
3. Dispatch one task per stage to the Architect, using the files in `dispatch/` as templates:
   environment paths, the evaluation command, and the full stage specification.
