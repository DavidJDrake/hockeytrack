package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	awslambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"

	"hockeytrack/internal/api"
	"hockeytrack/internal/events"
	"hockeytrack/internal/lake"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/poller"
	"hockeytrack/internal/schedsync"
	"hockeytrack/internal/store"
	"hockeytrack/internal/sweeper"
)

type app struct {
	nhl      *nhl.Client
	store    *store.DynamoStore
	archive  *store.S3Archive
	site     store.Archive // nil unless SITE_BUCKET is set
	pub      *events.EventBridgePublisher
	lambdaCl *awslambdasvc.Client
	schedCl  *scheduler.Client
	pollCfg  poller.Config
	handoff  time.Duration
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envSeconds(key string, def int) time.Duration {
	n, err := strconv.Atoi(envOr(key, strconv.Itoa(def)))
	if err != nil {
		n = def
	}
	return time.Duration(n) * time.Second
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required env var", "key", key)
		os.Exit(1)
	}
	return v
}

func newApp(ctx context.Context) (*app, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	maxChains, _ := strconv.Atoi(envOr("MAX_CHAINS", "30"))
	var site store.Archive
	if b := os.Getenv("SITE_BUCKET"); b != "" {
		site = store.NewS3Archive(s3.NewFromConfig(awsCfg), b)
	}
	return &app{
		site:     site,
		nhl:      nhl.New(),
		store:    store.NewDynamoStore(dynamodb.NewFromConfig(awsCfg), mustEnv("GAMES_TABLE")),
		archive:  store.NewS3Archive(s3.NewFromConfig(awsCfg), mustEnv("RAW_BUCKET")),
		pub:      events.NewEventBridgePublisher(eventbridge.NewFromConfig(awsCfg), mustEnv("EVENT_BUS")),
		lambdaCl: awslambdasvc.NewFromConfig(awsCfg),
		schedCl:  scheduler.NewFromConfig(awsCfg),
		pollCfg: poller.Config{
			LiveInterval:    envSeconds("POLL_INTERVAL_SECONDS", 5),
			PregameInterval: envSeconds("PREGAME_INTERVAL_SECONDS", 30),
			LeaseTTL:        60 * time.Second,
			MaxChains:       maxChains,
		},
		handoff: envSeconds("HANDOFF_BUFFER_SECONDS", 60),
	}, nil
}

func (a *app) pollerDeps() poller.Deps {
	return poller.Deps{
		Feed: a.nhl, Store: a.store, Archive: a.archive, Pub: a.pub,
		Now: time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		},
	}
}

func (a *app) handlePoller(ctx context.Context, raw json.RawMessage) error {
	gameID, err := parsePollerPayload(raw)
	if err != nil {
		return err
	}
	owner := "local"
	if lc, ok := lambdacontext.FromContext(ctx); ok {
		owner = lc.AwsRequestID
	}
	outcome, err := poller.Run(ctx, a.pollerDeps(), a.pollCfg, gameID, owner, shouldHandOff(ctx, a.handoff, time.Now))
	if err != nil {
		return err
	}
	slog.Info("poller finished", "gameId", gameID, "outcome", outcome)
	if outcome == poller.OutcomeHandOff {
		inv := sweeper.NewLambdaInvoker(a.lambdaCl, mustEnv("POLLER_FUNCTION_NAME"))
		return inv.InvokePoller(ctx, gameID)
	}
	return nil
}

func (a *app) handleScheduleSync(ctx context.Context) error {
	d := schedsync.Deps{
		Feed: a.nhl, Standings: a.nhl, Store: a.store, Archive: a.archive, Site: a.site,
		Scheduler: schedsync.NewAWSScheduler(a.schedCl, mustEnv("SCHEDULER_GROUP"), mustEnv("POLLER_FUNCTION_ARN"), mustEnv("SCHEDULER_ROLE_ARN")),
		Now:       time.Now,
	}
	// 300 days from any run date covers the rest of the season including playoffs.
	cfg := schedsync.Config{PregameBuffer: 15 * time.Minute, Horizon: 300 * 24 * time.Hour}
	return schedsync.Sync(ctx, d, cfg, time.Now().UTC().Format("2006-01-02"))
}

func (a *app) handleSweeper(ctx context.Context) error {
	inv := sweeper.NewLambdaInvoker(a.lambdaCl, mustEnv("POLLER_FUNCTION_NAME"))
	return sweeper.Sweep(ctx, a.store, inv, time.Now())
}

// query holds what the two read-side modes need: the raw archive, the
// index and the site bucket, and nothing the ingest path uses.
type query struct {
	raw   *store.S3Archive
	index *store.S3Archive
	site  *store.S3Archive // nil unless SITE_BUCKET is set
}

func newQuery(ctx context.Context) (*query, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	s3c := s3.NewFromConfig(awsCfg)
	q := &query{
		raw:   store.NewS3Archive(s3c, mustEnv("RAW_BUCKET")),
		index: store.NewS3Archive(s3c, mustEnv("INDEX_BUCKET")),
	}
	if b := os.Getenv("SITE_BUCKET"); b != "" {
		q.site = store.NewS3Archive(s3c, b)
	}
	return q, nil
}

// handleIndexer runs one indexing job: a game's summary plus its season's
// rollup after a final, or a whole season's sync on the daily schedule.
func (q *query) handleIndexer(ctx context.Context, raw json.RawMessage) error {
	job, err := parseIndexerPayload(raw)
	if err != nil {
		return err
	}
	ix := &lake.Indexer{Raw: q.raw, Index: q.index, Now: time.Now}
	if job.Prefix != "" {
		season, _, gameID, ok := lake.ParseGamePrefix(job.Prefix)
		if !ok {
			return fmt.Errorf("not a game prefix: %q", job.Prefix)
		}
		if _, err := ix.BuildGame(ctx, job.Prefix); err != nil {
			return fmt.Errorf("index game %d: %w", gameID, err)
		}
		r, err := ix.BuildSeason(ctx, season)
		if err != nil {
			return fmt.Errorf("rollup season %d: %w", season, err)
		}
		slog.Info("indexed game", "gameId", gameID, "season", season, "games", len(r.Games))
		return nil
	}
	season := job.Season
	if season == 0 {
		season = lake.CurrentSeason(time.Now())
	}
	st, err := ix.SyncSeason(ctx, season)
	slog.Info("season sync", "season", season, "stats", st)
	return err
}

func main() {
	ctx := context.Background()
	mode := mustEnv("MODE")
	if mode == "api" || mode == "indexer" {
		q, err := newQuery(ctx)
		if err != nil {
			slog.Error("init failed", "err", err)
			os.Exit(1)
		}
		if mode == "api" {
			deps := api.Deps{Raw: q.raw, Index: q.index, Now: time.Now, Version: os.Getenv("IMAGE_TAG")}
			if q.site != nil { // a nil pointer must not become a non-nil interface
				deps.Site = q.site
			}
			lambda.Start(api.LambdaURLHandler(api.New(deps)))
		} else {
			lambda.Start(q.handleIndexer)
		}
		return
	}
	a, err := newApp(ctx)
	if err != nil {
		slog.Error("init failed", "err", err)
		os.Exit(1)
	}
	switch mode {
	case "poller":
		lambda.Start(a.handlePoller)
	case "schedule-sync":
		lambda.Start(func(ctx context.Context) error { return a.handleScheduleSync(ctx) })
	case "sweeper":
		lambda.Start(func(ctx context.Context) error { return a.handleSweeper(ctx) })
	default:
		fmt.Fprintf(os.Stderr, "unknown MODE %q\n", mode)
		os.Exit(1)
	}
}
