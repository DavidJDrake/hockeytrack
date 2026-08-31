package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type DynamoStore struct {
	client *dynamodb.Client
	table  string
	Now    func() time.Time
}

func NewDynamoStore(client *dynamodb.Client, table string) *DynamoStore {
	return &DynamoStore{client: client, table: table, Now: time.Now}
}

// ddbGame is the marshaled shape of a GameRecord.
type ddbGame struct {
	GameID            int64             `dynamodbav:"gameId"`
	Season            int64             `dynamodbav:"season"`
	GameDate          string            `dynamodbav:"gameDate"`
	StartTimeUTC      string            `dynamodbav:"startTimeUTC"`
	HomeAbbrev        string            `dynamodbav:"homeAbbrev"`
	AwayAbbrev        string            `dynamodbav:"awayAbbrev"`
	Venue             string            `dynamodbav:"venue"`
	GameState         string            `dynamodbav:"gameState"`
	ScheduleEntryName string            `dynamodbav:"scheduleEntryName"`
	LastPlaySortOrder int64             `dynamodbav:"lastPlaySortOrder"`
	SnapshotHashes    map[string]string `dynamodbav:"snapshotHashes,omitempty"`
	ChainCount        int               `dynamodbav:"chainCount"`
	LeaseOwner        string            `dynamodbav:"leaseOwner,omitempty"`
	LeaseExpiresAt    string            `dynamodbav:"leaseExpiresAt,omitempty"`
	Done              bool              `dynamodbav:"done"`
}

func toRecord(g ddbGame) GameRecord {
	start, _ := time.Parse(time.RFC3339, g.StartTimeUTC)
	lease, _ := time.Parse(time.RFC3339, g.LeaseExpiresAt)
	return GameRecord{
		GameID: g.GameID, Season: g.Season, GameDate: g.GameDate,
		StartTimeUTC: start, HomeAbbrev: g.HomeAbbrev, AwayAbbrev: g.AwayAbbrev,
		Venue: g.Venue, GameState: g.GameState, ScheduleEntryName: g.ScheduleEntryName,
		LastPlaySortOrder: g.LastPlaySortOrder, SnapshotHashes: g.SnapshotHashes,
		ChainCount: g.ChainCount, LeaseOwner: g.LeaseOwner, LeaseExpiresAt: lease,
		Done: g.Done,
	}
}

func (d *DynamoStore) key(gameID int64) map[string]types.AttributeValue {
	k, _ := attributevalue.MarshalMap(map[string]int64{"gameId": gameID})
	return k
}

func (d *DynamoStore) UpsertSchedule(ctx context.Context, rec GameRecord) error {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.table),
		Key:       d.key(rec.GameID),
		UpdateExpression: aws.String(
			"SET season=:se, gameDate=:gd, startTimeUTC=:st, homeAbbrev=:h, awayAbbrev=:a, " +
				"venue=:v, gameState=:gs, scheduleEntryName=:sn, " +
				"lastPlaySortOrder=if_not_exists(lastPlaySortOrder,:zero), " +
				"chainCount=if_not_exists(chainCount,:zero), done=if_not_exists(done,:f)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":se":   &types.AttributeValueMemberN{Value: itoa(rec.Season)},
			":gd":   &types.AttributeValueMemberS{Value: rec.GameDate},
			":st":   &types.AttributeValueMemberS{Value: rec.StartTimeUTC.UTC().Format(time.RFC3339)},
			":h":    &types.AttributeValueMemberS{Value: rec.HomeAbbrev},
			":a":    &types.AttributeValueMemberS{Value: rec.AwayAbbrev},
			":v":    &types.AttributeValueMemberS{Value: rec.Venue},
			":gs":   &types.AttributeValueMemberS{Value: rec.GameState},
			":sn":   &types.AttributeValueMemberS{Value: rec.ScheduleEntryName},
			":zero": &types.AttributeValueMemberN{Value: "0"},
			":f":    &types.AttributeValueMemberBOOL{Value: false},
		},
	})
	return err
}

func (d *DynamoStore) Get(ctx context.Context, gameID int64) (*GameRecord, error) {
	out, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.table), Key: d.key(gameID), ConsistentRead: aws.Bool(true),
	})
	if err != nil || out.Item == nil {
		return nil, err
	}
	var g ddbGame
	if err := attributevalue.UnmarshalMap(out.Item, &g); err != nil {
		return nil, err
	}
	rec := toRecord(g)
	return &rec, nil
}

func (d *DynamoStore) ListByDate(ctx context.Context, date string) ([]GameRecord, error) {
	out, err := d.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(d.table),
		IndexName:              aws.String("byGameDate"),
		KeyConditionExpression: aws.String("gameDate = :d"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":d": &types.AttributeValueMemberS{Value: date},
		},
	})
	if err != nil {
		return nil, err
	}
	var recs []GameRecord
	for _, item := range out.Items {
		var g ddbGame
		if err := attributevalue.UnmarshalMap(item, &g); err != nil {
			return nil, err
		}
		recs = append(recs, toRecord(g))
	}
	return recs, nil
}

func (d *DynamoStore) AcquireLease(ctx context.Context, gameID int64, owner string, until time.Time) (bool, error) {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(d.table),
		Key:                 d.key(gameID),
		UpdateExpression:    aws.String("SET leaseOwner=:o, leaseExpiresAt=:u"),
		ConditionExpression: aws.String("attribute_exists(gameId) AND (attribute_not_exists(leaseOwner) OR leaseExpiresAt < :now OR leaseOwner = :o)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":o":   &types.AttributeValueMemberS{Value: owner},
			":u":   &types.AttributeValueMemberS{Value: until.UTC().Format(time.RFC3339)},
			":now": &types.AttributeValueMemberS{Value: d.Now().UTC().Format(time.RFC3339)},
		},
	})
	return leaseResult(err)
}

func (d *DynamoStore) RenewLease(ctx context.Context, gameID int64, owner string, until time.Time) (bool, error) {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(d.table),
		Key:                 d.key(gameID),
		UpdateExpression:    aws.String("SET leaseExpiresAt=:u"),
		ConditionExpression: aws.String("leaseOwner = :o"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":o": &types.AttributeValueMemberS{Value: owner},
			":u": &types.AttributeValueMemberS{Value: until.UTC().Format(time.RFC3339)},
		},
	})
	return leaseResult(err)
}

func (d *DynamoStore) ReleaseLease(ctx context.Context, gameID int64, owner string) error {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(d.table),
		Key:                 d.key(gameID),
		UpdateExpression:    aws.String("SET leaseExpiresAt=:zero"),
		ConditionExpression: aws.String("leaseOwner = :o"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":o":    &types.AttributeValueMemberS{Value: owner},
			":zero": &types.AttributeValueMemberS{Value: time.Time{}.UTC().Format(time.RFC3339)},
		},
	})
	if ok, err2 := leaseResult(err); !ok && err2 == nil {
		return nil // stale release: condition failed, treat as no-op
	}
	return err
}

func (d *DynamoStore) UpdatePollerState(ctx context.Context, gameID int64, st PollerState) error {
	hashes := st.SnapshotHashes
	if hashes == nil {
		hashes = map[string]string{}
	}
	hv, err := attributevalue.Marshal(hashes)
	if err != nil {
		return err
	}
	_, err = d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(d.table),
		Key:              d.key(gameID),
		UpdateExpression: aws.String("SET lastPlaySortOrder=:so, snapshotHashes=:sh, chainCount=:cc, gameState=:gs, done=:dn"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":so": &types.AttributeValueMemberN{Value: itoa(st.LastPlaySortOrder)},
			":sh": hv,
			":cc": &types.AttributeValueMemberN{Value: itoa(int64(st.ChainCount))},
			":gs": &types.AttributeValueMemberS{Value: st.GameState},
			":dn": &types.AttributeValueMemberBOOL{Value: st.Done},
		},
	})
	return err
}

// leaseResult maps ConditionalCheckFailed to (false, nil).
func leaseResult(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return false, nil
	}
	return false, err
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
