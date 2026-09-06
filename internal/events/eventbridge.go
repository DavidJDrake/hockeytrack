package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

type EventBridgePublisher struct {
	client  *eventbridge.Client
	busName string
	source  string
}

func NewEventBridgePublisher(client *eventbridge.Client, busName string) *EventBridgePublisher {
	return NewEventBridgePublisherWithSource(client, busName, Source)
}

// NewEventBridgePublisherWithSource publishes under an explicit source.
// Live-fire replays use SourceSynthetic so notification rules, which all
// pin the real source, cannot match them.
func NewEventBridgePublisherWithSource(client *eventbridge.Client, busName, source string) *EventBridgePublisher {
	return &EventBridgePublisher{client: client, busName: busName, source: source}
}

func (p *EventBridgePublisher) Publish(ctx context.Context, detailType string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	out, err := p.client.PutEvents(ctx, &eventbridge.PutEventsInput{
		Entries: []types.PutEventsRequestEntry{{
			EventBusName: aws.String(p.busName),
			Source:       aws.String(p.source),
			DetailType:   aws.String(detailType),
			Detail:       aws.String(string(b)),
		}},
	})
	if err != nil {
		return err
	}
	if out.FailedEntryCount > 0 {
		return fmt.Errorf("eventbridge put failed: %s", aws.ToString(out.Entries[0].ErrorMessage))
	}
	return nil
}
