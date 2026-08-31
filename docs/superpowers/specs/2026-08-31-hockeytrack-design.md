# HockeyTrack — NHL Game Data Ingestion Pipeline

**Date:** 2026-08-31
**Status:** Approved design, pre-implementation

## Purpose

Capture NHL game data continuously and durably, with near-real-time (seconds)
propagation of in-game events to downstream consumers. The immediate goal is
retention: store everything raw so any future use case has the data it needs.
The extensibility seam is EventBridge: future consumers (notifications,
dashboards, analytics) subscribe by rule pattern and never require pipeline
changes.

## Decisions made

| Decision | Choice |
|---|---|
| Data source | NHL public API (`api-web.nhle.com`), free, no key required |
| Latency target | Near-real-time; 5-second poll interval during live games (configurable) |
| Data scope | Everything raw: play-by-play + boxscore polled live; full endpoint sweep (shifts, landing, etc.) at game end |
| Storage | S3 for raw JSON archive + DynamoDB for game state/bookkeeping |
| Backfill | Forward-only for v1; backfill is a separate later task reusing the same fetch code |
| Language | Go (single static binary, container image) |
| Compute | Lambda from container images in ECR, structured for later migration to ECS/Fargate |
| Orchestration | Approach A: EventBridge Scheduler one-time entries per game; self-chaining poller Lambda |
| IaC | Terraform |
| Environments | Single environment for v1; no dev/prod split |
| CI/CD | Out of scope for v1 |

### Why self-chaining Lambda (Approach A)

EventBridge Scheduler cannot fire more often than once per minute, and a Lambda
invocation caps at 15 minutes while a game runs ~2.5–3 hours. Alternatives
considered:

- **B — Step Functions loop:** better observability, but polling logic moves
  into ASL state-machine definition, weakening the move-to-ECS portability
  story; ~2x cost.
- **C — Fargate task per game:** no time ceiling, no chaining; the natural
  migration target. Deferred to keep v1 Lambda-first per requirements.

A costs roughly $0.05/game of Lambda time (≈ $60–80/season). Poll interval
does not change Lambda cost (billed wall-clock either way); it only changes
NHL API request volume, which is free.

## High-level architecture

```
EventBridge cron (daily, ~04:00 ET)
        │
        ▼
┌─────────────────┐    writes games     ┌──────────────┐
│ schedule-sync    │───────────────────▶│  DynamoDB     │
│ Lambda (Go)      │  creates one-time  │  games table  │
└─────────────────┘  schedules ─┐       └──────────────┘
                                ▼               ▲ state/diff
                    EventBridge Scheduler       │ bookkeeping
                    (one entry per game,        │
                     fires at puck drop − 15m)  │
                                │               │
                                ▼               │
                    ┌──────────────────┐        │
                    │ game-poller       │────────┘
                    │ Lambda (Go,       │
                    │ container image,  │──▶ S3 (raw JSON snapshots)
                    │ self-chaining)    │
                    └──────────────────┘
                                │
                                ▼
                    EventBridge custom bus ──▶ future consumers
```

One Go container image, three modes selected by `MODE` env var:

1. **schedule-sync** — daily EventBridge cron. Pulls the league schedule,
   upserts games into DynamoDB, creates/updates/deletes one-time EventBridge
   Scheduler entries (self-deleting after fire) at puck drop − 15 minutes.
   Reconciles reschedules and postponements against existing table state.
2. **game-poller** — fired per game by Scheduler. Polls play-by-play +
   boxscore every 5s; diffs against DynamoDB high-water mark; publishes new
   events to the custom bus; writes changed raw snapshots to S3. Shortly
   before the 15-minute Lambda limit, if the game is not final,
   asynchronously re-invokes itself and exits. At game end, runs the full
   archive sweep and marks the game done.
3. **sweeper** — EventBridge rate rule every 5 minutes. Finds games that
   should be live but have an expired lease; re-invokes the poller.
   Self-healing for broken chains within ~5 minutes.

## Components

### DynamoDB `games` table

On-demand billing. One item per game, keyed by `gameId`:

- **Schedule fields:** home/away teams, `gameDate`, `startTimeUTC`, venue,
  game state, EventBridge Scheduler entry name (for reconciliation on
  reschedule).
- **Poller bookkeeping:** `lastPlaySortOrder` (diff high-water mark),
  `lastSnapshotHash` per feed (skip unchanged S3 writes), `chainCount`,
  lease fields `leaseOwner` + `leaseExpiresAt` (renewed via conditional
  writes each poll cycle — the concurrency guard ensuring at most one
  poller chain per game).
- **GSI on `gameDate`** for schedule-sync reconciliation and sweeper's
  "what should be live now" query.

### S3 raw archive

```
s3://<bucket>/raw/{season}/{date}/{gameId}/pbp/{pollTimestamp}.json      ← on change only
s3://<bucket>/raw/{season}/{date}/{gameId}/boxscore/{pollTimestamp}.json ← on change only
s3://<bucket>/raw/{season}/{date}/{gameId}/final/{feedName}.json         ← archive sweep
s3://<bucket>/raw/schedule/{date}.json                                   ← daily schedule pulls
```

Timestamped snapshots, never overwrites — preserves feed-revision history
(scorer changes, disallowed goals). Hash-compare before write. No lifecycle
rules for v1 (~few GB/season).

### EventBridge event contract

Custom bus `hockeytrack`, source `hockeytrack.poller`. Detail-types:

1. **`nhl.game.status`** — game state transitions (pregame → live → period
   changes → final), with score.
2. **`nhl.game.play`** — one event per new play-by-play entry. Detail
   carries `schemaVersion`, `gameId`, `seq` (NHL sortOrder), `playType`,
   both team codes, acting/scoring team, period/clock, score, full raw play
   object under `raw`.
3. **`nhl.game.final`** — emitted once after archive sweep, with final
   score and S3 pointers. Trigger point for future post-game analytics.

Contract rules:

- Every event carries `schemaVersion: 1`; schema evolves by version bump.
- Delivery is **at-least-once**; consumers dedupe on `(gameId, seq)`.

Example consumer (no pipeline change needed): a rule matching
`{"detail": {"playType": ["goal"], "scoringTeam": ["TBL"]}}` targeting an
SNS topic gives Lightning goal notifications.

### Failure handling

- **Broken chain / crashed poller:** sweeper re-invokes within ~5 minutes
  based on expired lease.
- **Runaway-chain protection:** poller exits if the game is marked final,
  if `chainCount` exceeds ~30 links (≈7 hours), or if it cannot acquire the
  lease. If the game never reaches final by the cap, mark `stale` and emit
  an alert event.
- **NHL API flakiness:** exponential backoff within the poll loop; a
  transient failure is a missed cycle, not an error.
- **Async invoke failures:** Lambda async retries + SQS DLQ behind each of
  the three functions; CloudWatch alarms on DLQ depth and poller error rate
  → SNS email.
- **Postponements/reschedules:** schedule-sync reconciles daily; a poller
  waking for a postponed game sees the state and exits immediately.

## Repo layout

```
hockeytrack/
├── cmd/ingestor/main.go     # single entrypoint; MODE selects schedule-sync | poller | sweeper
├── internal/
│   ├── nhl/                 # NHL API client: schedule, pbp, boxscore, shifts, landing
│   ├── poller/              # poll loop, diffing, lease/chain logic — runtime-agnostic
│   ├── schedsync/           # schedule pull + Scheduler reconciliation
│   ├── sweeper/             # stale-lease detection and re-invoke
│   ├── store/               # S3 + DynamoDB adapters behind interfaces
│   └── events/              # event types (schemaVersion'd) + EventBridge publisher
├── Dockerfile               # multi-stage → static binary on scratch/distroless
├── terraform/               # ECR, 3 Lambdas, DynamoDB, S3, bus, scheduler group, IAM, DLQs, alarms
└── Makefile                 # build / push / deploy targets
```

**Portability property:** `internal/poller`'s loop is a plain
"poll, diff, publish, sleep" function taking a `shouldHandOff()` callback.
Lambda mode watches the 15-minute clock and self-chains; a future ECS mode
always returns false and runs to game end. Fargate migration = new thin
entrypoint, not a rewrite.

## Deployment

`make deploy` = build image → push to ECR (git-SHA tag) → `terraform apply`.
All three functions reference the same image with different `MODE` values.
Least-privilege IAM roles per function, including the `iam:PassRole`
schedule-sync needs to create Scheduler entries.

## Testing

- **Unit tests with real fixtures:** captured NHL API responses as testdata;
  golden tests for the diff logic ("given snapshot N and N+1, exactly these
  events are emitted") — the correctness core consumers rely on.
- **AWS adapters behind interfaces** with in-memory fakes; no localstack.
- **Replay harness:** replays a recorded game's snapshots through the full
  poller path against fakes. Primary validation during the off-season (no
  live games until preseason, late September).
- Manual smoke test of schedule-sync against the real API (works year-round).

## Out of scope for v1

- Historical backfill (later task, reuses fetch code)
- Dev/prod environment split
- CI/CD pipeline
- Any parsed/derived data stores (Athena, RDS, etc.)
- ECS/Fargate runtime (kept open by design, not built)
