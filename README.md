# Tablekeeper, built by a dark factory

Our entry for the [WeAreDevelopers x BAND Dark Factory hackathon](https://lablab.ai/ai-hackathons/wearedevelopers-hackathon).

- **Team:** Tey Yong Zhun
- **Track:** `tablekeeper`, a restaurant reservation system
- **Stage reached:** 4 of 4. A final `harness run --all --mode isolated` reports every folder
  claiming its own stage on the shipped checks.

A three-seat band in Band Desktop built this service one stage at a time. The human sent one
task per stage and nothing else; every line under `stage-N/` came out of the room.

## How to read this repository

| Path | What it is |
|---|---|
| [`FACTORY.md`](FACTORY.md) | The factory: seats, workflow, design choices and their cost, what failed, how bad work is caught, measured cost and time. **Start here** |
| [`mandates/`](mandates/) | One mandate per seat (`architect.md`, `developer.md`, `qa-lead.md`), each naming its harness and model. Generic: nothing in them names this track |
| [`room.json`](room.json) | The full Band room for the submitted run, downloaded unchanged |
| [`stage-1/`](stage-1/) … [`stage-4/`](stage-4/) | The service after each stage. Each folder is a complete service (Go, `Dockerfile`, `RUN.md`) carried forward from the one before |
| [`dispatch/`](dispatch/) | The exact task sent to the band for each stage, plus the toy rehearsal |
| [`agents/`](agents/) | The runner for the two OpenCode seats (`seat.py`, `architect.py`, `qa_lead.py`) |
| [`SETUP.md`](SETUP.md) | Step-by-step setup to stand the factory up again |

## The band

| Seat | Role | Harness | Model |
|---|---|---|---|
| Architect | Lead: plans, writes `DESIGN.md`, hands off, routes review, reports | OpenCode | `MiniMaxAI/MiniMax-M2.5` (Featherless) |
| Developer | Implements, builds the container, commits | Claude Code | `claude-sonnet-5-5` |
| QA Lead | Independent review: acceptance tests, clean builds, harness runs, PASS/REJECT | OpenCode | `MiniMaxAI/MiniMax-M2.5` (Featherless) |

## Running a stage

Each folder's `RUN.md` has the exact commands. For example:

```sh
cd stage-4
docker build -t tablekeeper .
docker run --rm -e PORT=8080 -p 8080:8080 tablekeeper
```

Then open <http://localhost:8080> for the browser UI (from stage 2 on), or call the JSON API.

## Results in brief

| Stage | Outcome | Featherless credit | Wall time |
|---|---|---|---|
| 1 | PASS after QA rejected 10 failing shipped checks (three root causes) | $0.40 | 39 min |
| 2 | PASS | $0.12 | 23 min |
| 3 | PASS after QA rejected two spec deviations the shipped checks missed | $0.23 | 28 min |
| 4 | PASS | $0.13 | 14 min |

Details, including Claude usage and what we learned along the way, are in
[`FACTORY.md`](FACTORY.md).

## License

[MIT](LICENSE)
