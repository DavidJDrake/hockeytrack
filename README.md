# HockeyTrack

A serverless NHL game-data ingestion pipeline on AWS. It watches the league schedule, wakes up for every game, polls the NHL's public API every few seconds while the puck is in play, archives every raw response to S3, and publishes each play as a discrete event on an EventBridge bus — so anything you want to build on live hockey data is one EventBridge rule away.

## What it does

- **Archives everything, raw.** Play-by-play and boxscore snapshots land in S3 whenever they change during a game, plus a full end-of-game sweep (play-by-play, boxscore, landing summary, shift charts). Nothing is parsed away; every future use case has the original JSON.
- **Publishes events in near-real-time.** Every new play (goals, shots, hits, penalties, faceoffs…), every game-state change, and a final summary event go onto a custom EventBridge bus within seconds of the NHL API reflecting them.
- **Runs only when hockey is on.** A daily job walks the entire published season and keeps a one-time EventBridge Scheduler entry per game — every game is armed as soon as the league announces it, and reschedules are reconciled each morning. No games, no compute.

## Architecture

<p align="center">
  <img src="docs/architecture.svg" alt="HockeyTrack architecture: schedule-sync records every game in DynamoDB and creates a Scheduler entry per game; at puck drop the entry starts a game-poller that polls the NHL API every 5 seconds, archives raw JSON to S3, and publishes play events to an EventBridge bus consumed by your own rules; a sweeper restarts any poller that dies mid-game." width="960">
</p>

One Go binary, shipped as a single container image, runs in three modes selected by a `MODE` env var:

| Mode | Trigger | Job |
|---|---|---|
| `schedule-sync` | daily cron | Pull the full published season (following the API's week-to-week links), record games in DynamoDB, create/move/delete per-game Scheduler entries (handles reschedules and postponements) |
| `poller` | per-game Scheduler entry | Poll play-by-play + boxscore every 5s, diff against a DynamoDB high-water mark, publish new events, snapshot changed JSON to S3; near the 15-minute Lambda limit it re-invokes itself and hands off (a DynamoDB lease guarantees exactly one active chain per game); at game end it runs the archive sweep |
| `sweeper` | 5-minute rate rule | Restart any poller chain that died mid-game, detected via expired leases |

The poll loop itself is runtime-agnostic — Lambda's 15-minute ceiling is handled by an injected `shouldHandOff()` callback. Point the same container at ECS/Fargate with a callback that always returns false and it simply polls a whole game in one run. That migration is a new thin entrypoint, not a rewrite.

## The website

**[hockeytrack.davidjdrake.com](https://hockeytrack.davidjdrake.com)** shows the whole season — every game, with **multi-select filters for teams and months** (pick any combination of clubs to see every game involving them, and any set of months; selections show as removable chips and persist between visits), a preseason/regular-season toggle, and free-text search. Selecting a single team switches to that club's season view: game number 1–84, home/away, rest days, and back-to-backs. It is a static page on S3 behind CloudFront; the schedule it renders is `data/schedule.json`, which `schedule-sync` republishes every morning, so it stays current with reschedules without any manual step. The page lives in `site/` and is uploaded with `make site`; the infrastructure (bucket, distribution, certificate, DNS) is `terraform/site.tf`.

## The event contract

Bus `hockeytrack`, source `hockeytrack.poller`. Three detail-types (plus `hockeytrack.alert` for operational alerts):

- **`nhl.game.play`** — one event per play. Detail includes `schemaVersion`, `gameId`, `seq`, `playType` (`goal`, `shot-on-goal`, `penalty`, `faceoff`, `hit`, …), team abbreviations, `scoringTeam` (goals only), period/clock, the running score, and the full untouched NHL play object under `raw`.
- **`nhl.game.status`** — game-state transitions (pregame → live → final) with score.
- **`nhl.game.final`** — emitted once after the archive sweep, with the final score and the game's S3 prefix.

Delivery is at-least-once; consumers should dedupe on `(gameId, seq)`. Every event carries `schemaVersion` so the schema can evolve without breaking you.

**`raw` is untrusted input.** It is third-party JSON from the NHL API passed through verbatim — HockeyTrack does not validate, sanitize, or bound it. Treat it as you would any external payload: validate the fields you use, escape it before rendering it in HTML/SMS/email, never `eval` or template it unescaped, and don't assume its shape is stable. The typed top-level fields (`playType`, `scoringTeam`, score, period/clock) are parsed by the poller and are safer to match rules on, but their string values still originate from the same source.

## Extending it

The pipeline never needs to change for new consumers — subscribe with an EventBridge rule and point it at any target (Lambda, SNS, SQS, Step Functions, API destinations…).

**"Text me every Lightning goal":**

```json
{
  "source": ["hockeytrack.poller"],
  "detail-type": ["nhl.game.play"],
  "detail": { "playType": ["goal"], "scoringTeam": ["TBL"] }
}
```
→ target an SNS topic with an SMS/email subscription. That's the whole feature.

**Other natural extensions:**

- **Post-game analytics** — rule on `nhl.game.final`, trigger a Lambda/Glue job that reads the game's S3 prefix (the event hands it to you).
- **Live dashboards** — rule on `nhl.game.play` → WebSocket pushes via API Gateway.
- **Historical backfill** — the `internal/nhl` client fetches any past game; a batch job reusing it can fill S3 with prior seasons.
- **Ad-hoc queries over the archive** — the S3 layout (`raw/{season}/{date}/{gameId}/{feed}/…`) partitions cleanly for Athena.
- **Fargate migration** — see above; the container is already ECS-ready.

## Deploying

Prereqs: AWS credentials, Terraform ≥ 1.10 (the S3 backend uses native `use_lockfile` state locking; there is no DynamoDB lock table), Docker, Go 1.23+.

```bash
# one-time: create terraform/terraform.tfvars (gitignored) with your email variables,
# then create the ECR repo first so the image push has a home
cd terraform && terraform init && terraform apply -target=aws_ecr_repository.main -var="image_tag=bootstrap"

# then every deploy: test → build → push image (tagged with the git SHA) → terraform apply
make deploy
```

Variables: `region` (default `us-east-1`), `image_tag` (set by `make deploy`), and two **required** email variables that have no default on purpose: `alert_email` (subscribes you to CloudWatch alarms for DLQ depth — the three Lambda DLQs and the DLQ behind the ECR scan-findings EventBridge target — and poller errors, and to ECR scan results with CRITICAL/HIGH findings) and `lightning_email` (the example goal-alert rule in `terraform/notifications.tf`). Set either to `""` to opt out of that subscription. Put them in a gitignored `terraform/terraform.tfvars`, which Terraform loads automatically so `make deploy` carries them every time:

```hcl
alert_email     = "you@example.com"
lightning_email = ""
```

They are required rather than defaulting to empty because a plan run from a checkout without the tfvars file would otherwise *silently* plan to destroy the existing email subscriptions; with no default, Terraform refuses to plan until the values are supplied.

**Logging and WAF.** CloudFront standard logging for the website is deliberately not enabled: the site is static, has no authentication or user data, and an access log bucket adds S3 cost and another data store to manage for little investigative value at this scale. AWS WAF is likewise not warranted for a static site of this size. Both can be added later in `terraform/site.tf` (a private log bucket plus the distribution's `logging_config` block; a WAF web ACL attached via `web_acl_id`) if abuse or traffic patterns ever justify them. The Lambdas do log to CloudWatch, and each function's role may write only to its own `/aws/lambda/<function>` log group.

Cost is dominated by poller runtime: roughly **$0.05/game**, on the order of **$10–15/month in season** and near-zero in the off-season. S3/DynamoDB/EventBridge usage is pennies at this volume.

## Development

- `make test` / `go test ./...` — unit tests use real captured NHL API responses as fixtures (never hand-written), with golden tests on the play-by-play diff logic: given snapshot N and N+1, exactly these events are emitted. `make test` first runs `make vuln` (govulncheck), so a known reachable vulnerability fails the build.
- **Supply chain** — every image pushed to ECR is scanned on push; the `Dockerfile` pins both base images by digest (bump instructions are in its header comment). ECR keeps only the 20 most recently pushed tagged images (`ecr_keep_images` in `terraform/variables.tf`) and drops untagged ones after a day, so superseded release tags stop accumulating. Tags are immutable and every deploy pushes a fresh git-SHA tag, so the image the Lambdas run is always among the newest and never ages out.
- **Replay harness** — run a full recorded game through the real poller path against in-memory fakes, printing every event it would publish:

  ```bash
  go run ./cmd/replay -game path/to/snapshots/
  ```

  This is the primary end-to-end check when no live games are on.
- AWS is always behind interfaces (`internal/store`, `internal/events`); logic tests run against fakes, no AWS or localstack required.
- **Link previews** — `make og` re-renders the home page's Open Graph card (`site/assets/og/home.png`) from `docs/og/home.html` with the puck-drop countdown as of that moment. The ice photo is by Compagnons on Unsplash (Unsplash License).

### Backfilling history

The pipeline is forward-only, but the NHL API serves every game back to the league's first on 19 December 1917. `cmd/backfill` walks a past season's schedule week by week and writes the same `final/` objects the poller leaves behind (play-by-play, boxscore, landing, and shift charts from 2010-11 onward) for every finished game. It touches only S3: no events are published, so a historical Lightning goal never trips a notification rule, and nothing is written to DynamoDB or the scheduler.

```bash
make backfill                     # every season the API lists, newest first
make backfill SEASONS=20252026    # one season
make backfill SEASONS=19171918-19421943
```

Runs are resumable and idempotent: objects already in the bucket are skipped, so an interrupted run (or one with transient failures) is just rerun. Requests are paced at 3/s by default (`RPS=`). What you get depends on the era:

| Seasons | Play-by-play | Shift charts |
|---|---|---|
| 2010-11 onward | Full, with coordinates | Yes |
| 2009-10 | Full, with coordinates | No |
| 2006-07 to 2008-09 | Goals, shots, penalties | No |
| 1917-18 to 2005-06 | Goals and penalties | No |

The whole history is roughly 70,000 games, 235,000 requests, and 20 GB, which is about a day of wall time at the default pace and under $0.50/month to keep.

```
cmd/ingestor/     entrypoint; MODE selects schedule-sync | poller | sweeper
cmd/backfill/     historical season backfill (local CLI, S3 only)
cmd/replay/       offline replay harness
site/             the website (static page; data published by schedule-sync)
internal/nhl/     NHL API client + captured fixtures
internal/poller/  diff logic + the runtime-agnostic poll loop
internal/backfill/  season walk + final-feed fetch with pacing, retry, resume
internal/schedsync/  schedule pull + Scheduler reconciliation
internal/sweeper/ dead-chain detection and restart
internal/store/   DynamoDB game store (leases, high-water marks) + S3 archive
internal/events/  versioned event types + EventBridge publisher
terraform/        the whole stack: ECR, Lambdas, DynamoDB, S3, bus, scheduler, IAM, DLQs, alarms
docs/superpowers/ design spec and implementation plan
```

## Caveats

- The NHL API (`api-web.nhle.com`) is **unofficial and undocumented**. It's free, keyless, and widely used by community projects, but the NHL could change or restrict it at any time. The client isolates all API knowledge in `internal/nhl`, and the raw archive means a format change never costs you already-captured data.
- This project is not affiliated with or endorsed by the NHL. Data belongs to its respective owners; use responsibly.

## License

[MIT](LICENSE) — use it, fork it, build your own alerts on it.
