package schedsync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/scheduler/types"
)

type AWSScheduler struct {
	client    *scheduler.Client
	group     string
	targetArn string // poller Lambda ARN
	roleArn   string // role EventBridge Scheduler assumes to invoke it
}

func NewAWSScheduler(client *scheduler.Client, group, targetArn, roleArn string) *AWSScheduler {
	return &AWSScheduler{client: client, group: group, targetArn: targetArn, roleArn: roleArn}
}

func (s *AWSScheduler) Ensure(ctx context.Context, name string, fireAt time.Time, gameID int64) error {
	target := &types.Target{
		Arn:     aws.String(s.targetArn),
		RoleArn: aws.String(s.roleArn),
		Input:   aws.String(fmt.Sprintf(`{"gameId":%d}`, gameID)),
	}
	in := scheduler.CreateScheduleInput{
		Name:                  aws.String(name),
		GroupName:             aws.String(s.group),
		ScheduleExpression:    aws.String("at(" + fireAt.UTC().Format("2006-01-02T15:04:05") + ")"),
		FlexibleTimeWindow:    &types.FlexibleTimeWindow{Mode: types.FlexibleTimeWindowModeOff},
		Target:                target,
		ActionAfterCompletion: types.ActionAfterCompletionDelete,
	}
	_, err := s.client.CreateSchedule(ctx, &in)
	var conflict *types.ConflictException
	if errors.As(err, &conflict) {
		_, err = s.client.UpdateSchedule(ctx, &scheduler.UpdateScheduleInput{
			Name: in.Name, GroupName: in.GroupName, ScheduleExpression: in.ScheduleExpression,
			FlexibleTimeWindow: in.FlexibleTimeWindow, Target: in.Target,
			ActionAfterCompletion: in.ActionAfterCompletion,
		})
	}
	return err
}

func (s *AWSScheduler) Delete(ctx context.Context, name string) error {
	_, err := s.client.DeleteSchedule(ctx, &scheduler.DeleteScheduleInput{
		Name: aws.String(name), GroupName: aws.String(s.group),
	})
	var nf *types.ResourceNotFoundException
	if errors.As(err, &nf) {
		return nil
	}
	return err
}
