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

	"hockeytrack/internal/events"
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
	return &app{
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
		Feed: a.nhl, Store: a.store, Archive: a.archive,
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

func main() {
	ctx := context.Background()
	a, err := newApp(ctx)
	if err != nil {
		slog.Error("init failed", "err", err)
		os.Exit(1)
	}
	switch mode := mustEnv("MODE"); mode {
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
