// Package sweeper restarts poller chains that died mid-game, detected via
// expired leases.
package sweeper

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"hockeytrack/internal/store"
)

type Invoker interface {
	InvokePoller(ctx context.Context, gameID int64) error
}

type FakeInvoker struct {
	Invoked []int64
	Err     error
}

func (f *FakeInvoker) InvokePoller(_ context.Context, gameID int64) error {
	if f.Err != nil {
		return f.Err
	}
	f.Invoked = append(f.Invoked, gameID)
	return nil
}

func Sweep(ctx context.Context, st store.GameStore, inv Invoker, now time.Time) error {
	dates := []string{
		now.UTC().Format("2006-01-02"),
		now.UTC().AddDate(0, 0, -1).Format("2006-01-02"),
	}
	for _, date := range dates {
		games, err := st.ListByDate(ctx, date)
		if err != nil {
			return err
		}
		for _, g := range games {
			if g.Done || g.GameState == "FINAL" || g.GameState == "OFF" {
				continue
			}
			if now.Before(g.StartTimeUTC.Add(-16 * time.Minute)) {
				continue // entry hasn't fired yet
			}
			if now.After(g.StartTimeUTC.Add(6 * time.Hour)) {
				continue // give up; max-chain alerting covers pathological games
			}
			if g.LeaseOwner != "" && g.LeaseExpiresAt.After(now) {
				continue // a poller link is alive
			}
			slog.Info("sweeper restarting poller", "gameId", g.GameID, "state", g.GameState)
			if err := inv.InvokePoller(ctx, g.GameID); err != nil {
				return err
			}
		}
	}
	return nil
}

type LambdaInvoker struct {
	client       *awslambda.Client
	functionName string
}

func NewLambdaInvoker(client *awslambda.Client, functionName string) *LambdaInvoker {
	return &LambdaInvoker{client: client, functionName: functionName}
}

func (l *LambdaInvoker) InvokePoller(ctx context.Context, gameID int64) error {
	payload, _ := json.Marshal(map[string]int64{"gameId": gameID})
	out, err := l.client.Invoke(ctx, &awslambda.InvokeInput{
		FunctionName:   aws.String(l.functionName),
		InvocationType: lambdatypes.InvocationTypeEvent,
		Payload:        payload,
	})
	if err != nil {
		return err
	}
	if out.StatusCode < 200 || out.StatusCode >= 300 {
		return fmt.Errorf("async invoke status %d", out.StatusCode)
	}
	return nil
}
