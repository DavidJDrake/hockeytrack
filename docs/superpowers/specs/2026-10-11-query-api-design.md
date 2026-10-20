# A Query API over the Archive — Design

**Status:** built on branch `glados/23be6e29`, not yet reviewed or deployed (built autonomously from a one-line request; the assumptions in §2 are the ones Jay should check first)
**Date:** 2026-10-11
**Request:** "Build a full-featured API that allows searching the data lake, enabling queries about statistics, game schedules, and other hockey data."
**Repository:** HockeyTrack only.

## 1. Purpose

The archive holds every raw NHL feed the pipeline has ever fetched, partitioned by season, date and game. Today the only ways to ask it a question are `aws s3 cp`, `cmd/analyze` into CSV, or an EventBridge rule. This design adds a public, read-only, JSON HTTP API that answers the questions a hockey consumer actually asks: who plays tonight, what happened in a game, who leads the league in points, how a goalie is doing, where a team sits in the standings, and "find me X".

Success looks like `curl https://api.hockeytrack.davidjdrake.com/v1/stats/skaters?season=20252026&sort=points&limit=10` returning the scoring leaders in well under a second, and every endpoint being testable offline against the fixtures already in the repository.

## 2. Assumptions (not confirmed with Jay)

1. **Public and unauthenticated.** The site is public, the data is the NHL's public data, and the README invites people to build on it. Adding accounts and keys would be a different product. Abuse is bounded by caching and concurrency rather than by identity (§7).
2. **Read-only.** The API never writes to the raw bucket, DynamoDB or the bus. The only writer this design adds is the indexer, which writes to its own bucket.
3. **Freshness of minutes, not seconds.** Live games are visible through their newest snapshot, but season statistics refresh when a game ends, not on every poll.
4. **Same binary, same image, same deploy path.** Two new `MODE`s on the ingestor rather than a new service, following the README's "one Go binary" rule.
5. **The archive is the source of truth; the index is disposable.** Anything derived can be rebuilt from `raw/` with one command.

## 3. What the data lake holds, and what the API reads

| Object | Written by | Read by the API for |
|---|---|---|
| `raw/{season}/{date}/{gameId}/final/{pbp,boxscore,landing,shifts}.json` | poller, backfill | a finished game's summary, plays, boxscore, shifts, raw pass-through |
| `raw/{season}/{date}/{gameId}/{pbp,boxscore}/{ts}.json` | poller, during the game | a live game's newest state |
| `raw/standings/{date}.json` | schedule-sync, daily | standings on a date, and the latest |
| `data/schedule.json` (site bucket) | schedule-sync, daily | the full published season: every game, including those not yet played |
| `v1/seasons/{season}/games/{gameId}.json` (index bucket, new) | indexer | one game's summary: header, team lines, every skater and goalie line, scoring summary |
| `v1/seasons/{season}.json` (index bucket, new) | indexer | the season rollup: game list with results, per-(player, team) season totals, team totals |

Nothing is read from DynamoDB. The games table is poller bookkeeping; what the API needs from it (dates, teams, state) is in the schedule document and the archive.

## 4. Components

### 4.1 `internal/lake` — the derived model

Pure functions from raw feeds to typed, API-shaped values. No AWS. Tested on the real fixtures in `internal/nhl/testdata` and `internal/synth/testdata`.

- `Summarize(pbp, boxscore, landing, shifts []byte) (*GameSummary, error)`. Only `pbp` is required; the others add what they can. From the boxscore: every skater's and goalie's official line. From the play-by-play: faceoff wins and losses per player (the boxscore has only a percentage), the scoring summary, and a fallback skater line (goals, assists, PIM, shots) for eras with no boxscore stats. From the landing: three stars. From the shift chart: shift and time-on-ice counts when the boxscore lacks them.
- `Rollup(games []GameSummary) *SeasonRollup`. Sums lines per `(playerId, team)`, builds team records (W, L, OTL, T, PTS, ROW, GF, GA, SF, SA, PIM, PPG, hits, blocks), and lists every game with its result. A traded player has one row per team; the API sums rows when no team filter is given.
- `Feeds` helpers: locate a game's objects under its prefix, preferring `final/` and falling back to the newest timestamped snapshot, and report whether the game is live.
- `Index` key functions, and an `Indexer` that builds a game's summary from the raw archive and a season's rollup from the summaries, using the same `List`/`Get`/`Put` interfaces every other package uses.

### 4.2 `internal/api` — the HTTP layer

A `net/http` handler built from `Deps{Raw, Index, Site Source; Now}`. Routing uses the standard library's method-and-pattern mux. Responses are JSON; errors are `{"error":{"code","message"}}` with the matching status. Lists are paged with `limit` (default 100, max 1000) and `offset`, and carry `total`. Every response sets `Cache-Control` for CloudFront: a finished game's objects for a day, a live game for ten seconds, the schedule and statistics for five minutes, standings for an hour. CORS is `*` because the API is public and read-only.

An in-process TTL cache holds the parsed season rollup, the schedule document and the standings listing, so a warm Lambda answers most requests without touching S3.

The Lambda entry is a small adapter from the Function URL event (payload format 2.0) to `http.Request`/`http.ResponseWriter`, written here rather than taken as a dependency: it is a hundred lines, and the API then runs unchanged as a plain local server.

### 4.3 Endpoints (all `GET`, prefix `/v1`)

| Path | Answers | Source |
|---|---|---|
| `/v1` | links and the season the API treats as current | index listing |
| `/v1/openapi.json` | the OpenAPI 3.1 description of everything below | embedded |
| `/v1/seasons` | seasons the index knows (from the key listing, so a century of seasons costs one list call) | index listing |
| `/v1/seasons/{season}` | one season's extent: game counts, date range, teams | rollup |
| `/v1/schedule?date=\|from=&to=\|team=\|season=\|type=\|limit=&offset=` | games, past and future, with results attached when known; `date=today` is honored | schedule document + rollup |
| `/v1/teams` | every team in the current schedule, with name | schedule document |
| `/v1/teams/{abbrev}?season=` | record, standings row, last result, next game | rollup, standings, schedule |
| `/v1/teams/{abbrev}/games?season=` | that team's games with results | schedule document + rollup |
| `/v1/teams/{abbrev}/roster?season=` | everyone who dressed for the team that season, with totals | rollup |
| `/v1/standings?date=&conference=&division=` | the NHL's own table on a date (default: newest) | `raw/standings/` |
| `/v1/games?season=&date=&team=&type=&limit=&offset=` | finished games in the index, with results | rollup |
| `/v1/games/{id}` | header, team lines, goalies, scoring summary, three stars; `live:true` when served from a snapshot | summary or raw feeds |
| `/v1/games/{id}/plays?type=&team=&period=&player=&limit=&offset=` | typed plays with their untouched `details` | `pbp.json` |
| `/v1/games/{id}/boxscore` | every skater and goalie line | summary |
| `/v1/games/{id}/shifts?player=&team=&period=` | the shift chart as rows | `shifts.json` |
| `/v1/games/{id}/raw/{feed}` | the archived feed as the NHL sent it (`pbp`, `boxscore`, `landing`, `shifts`) | archive |
| `/v1/stats/skaters?season=&team=&position=&minGp=&sort=&order=&limit=&offset=` | leaderboard; sortable by any numeric column | rollup |
| `/v1/stats/goalies?season=&team=&minGp=&sort=&order=…` | leaderboard | rollup |
| `/v1/stats/teams?season=&sort=&order=` | team totals computed from the archive | rollup |
| `/v1/players/{id}?season=` | the player's season line(s) and teams | rollup |
| `/v1/players/{id}/games?season=` | game log | per-game summaries of the player's team |
| `/v1/search?q=&season=` | teams, players, venues and games matching a string | schedule document + rollup |
| `/healthz` | liveness | — |

Game lookup by id resolves the game's date (needed for the S3 prefix) from the rollup first, then the schedule document, then by listing the season prefix; that listing is cached per season, so a run of unknown ids costs one list call rather than one each. The id encodes the season, as `synth.SeasonOf` already relies on.

Two things the implementation settled that this design left open: a goalie who dressed but never played (the boxscore lists the backup with no time on ice, no shots and no decision) counts toward nothing in the rollup, and a shootout's deciding goal appears in the scoring summary but is nobody's goal in the statistics, both as the NHL counts them.

### 4.4 The indexer

`MODE=indexer` on the ingestor, invoked two ways:

- **On every `nhl.game.final`** (an EventBridge rule on the `hockeytrack` bus, source `hockeytrack.poller`): build that game's summary from the prefix the event carries, then rebuild its season rollup from every summary under the season. Statistics are current within about a minute of the final horn.
- **Daily at 10:00 UTC**, an hour after schedule-sync: build any summary missing for the current season (comparing `final/pbp.json` keys against summary keys), then rebuild the rollup. This catches a lost event, a race between two finals, and a backfill.

Two finals ending within the same few seconds can race on the rollup, and the later writer wins with whichever listing it saw; the daily rebuild repairs that. The rollup is always rebuilt whole from summaries rather than merged incrementally, so there is exactly one code path and it is idempotent.

`cmd/index` runs the same code locally against a bucket or a mirror, for history: `go run ./cmd/index -seasons 20242025` builds the summaries and rollups for every game the archive holds for that season, skipping summaries that already exist.

### 4.5 Infrastructure (`terraform/api.tf`)

- An **index bucket** `hockeytrack-index-{account}`: private, unversioned, rebuildable. Derived data does not belong in the raw archive, whose versioning, lifecycle, size alarm and tamper policy all assume write-once objects.
- **`hockeytrack-api`** Lambda (image, `MODE=api`, 512 MB, 30 s) with a **Function URL** in `AWS_IAM` auth mode, invokable only by the CloudFront distribution through an Origin Access Control of type `lambda`. Nothing reaches the function except through CloudFront, so caching and any future WAF rule are not bypassable.
- **CloudFront** at `api.hockeytrack.davidjdrake.com`: its own ACM certificate, a cache policy that keys on the full query string and honors origin `Cache-Control` (min 0, default 60 s, max 1 day), compression on, a response-headers policy with HSTS, nosniff, a restrictive CSP for a JSON origin, and no cookies or headers forwarded.
- **`hockeytrack-indexer`** Lambda (image, `MODE=indexer`, 1024 MB, 600 s), a DLQ, an errors alarm, an EventBridge rule on `nhl.game.final`, and a Scheduler entry at 10:00 UTC.
- **IAM:** the API role may `s3:GetObject`/`ListBucket` on the raw, index and site buckets and write its own log group. The indexer role may read and list the raw bucket and read, list and put on the index bucket. Neither touches DynamoDB, the bus, Scheduler or other functions.
- **Alarms:** API errors and indexer errors to `hockeytrack-alerts`; the indexer DLQ joins the depth alarm.

## 5. Error handling

- A game with no archived feed at all is 404. A game whose `pbp.json` is unreadable is 502 with a message naming the feed, never a crash: every raw field is parsed as untrusted (the `analyze` package's field-by-field decoding is the model).
- An unknown season, team or player is 404; a malformed parameter (bad date, non-numeric id, unknown sort column) is 400 with the parameter named.
- S3 failures are 503 with `Retry-After`; the handler logs the key, not the body.
- The raw pass-through sets `Content-Type: application/json` and `X-Content-Type-Options: nosniff`, and the README's warning that `raw` is untrusted third-party JSON applies to it verbatim.

## 6. Testing

- `internal/lake`: `Summarize` and `Rollup` against the three real fixture sets (a 2025 regular game with all four feeds, the eight synthetic-replay games including a shootout and a 1917 game with no roster), asserting known scores, scorer ids, faceoff counts and totals; a corrupted-field test proving a bad value blanks one cell and nothing else.
- `internal/api`: `httptest` against a `FakeArchive` loaded from the same fixtures, one test per endpoint for the happy path, the 404 path and the malformed-parameter path; pagination and sort stability; cache headers; the Function URL adapter round-trip including query strings, repeated parameters and a 404.
- `cmd/index` and `cmd/api` are thin and are exercised through a local run against a directory mirror in the README instructions.
- CI already runs `gofmt`, `go vet`, `go test`, `govulncheck`, `terraform fmt` and `terraform validate`; nothing new is needed.

## 7. Security and cost

- **Exposure:** public data only, no user input reaches S3 except through validated path segments (numeric ids, three-letter abbreviations, ISO dates, a fixed feed list). No query parameter is ever interpolated into a key.
- **The honest risk is concurrency, not data.** The account's Lambda concurrency is shared with the pollers, and a traffic spike on the API could starve them during a game. CloudFront caching absorbs repeated requests, and the Function URL cannot be hit directly, but a crawler walking distinct URLs still reaches the function. `api_reserved_concurrency` is a variable (default none) to cap the API once the account quota is above the AWS-required ten unreserved; until then this is an accepted risk and is written into the threat model. A WAF rate rule is the next step if traffic ever justifies it, exactly as `site.tf` reasons.
- **Cost:** CloudFront and Lambda at zero traffic are zero; the index bucket is a few MB per season; the indexer's rebuild after each final is on the order of 1,400 small reads, pennies a season. The S3 data-event trail is write-only by default, so API reads do not inflate CloudTrail; turning `cloudtrail_archive_read_events` on now also logs the API's reads, which the variable description says.
- **What it does not do:** rate limiting per client, authentication, or any write. Each is a deliberate omission for a public read-only API at this scale.

## 8. Out of scope

Athena and Glue (the index replaces the need for them at this size), per-player historical career totals across seasons in one call (the client can walk `/v1/seasons`), WebSocket or push, and any change to the poller, schedule-sync, sweeper, backfill or the event contract.
