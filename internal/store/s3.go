package store

import (
	"bytes"
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Archive struct {
	client *s3.Client
	bucket string
}

func NewS3Archive(client *s3.Client, bucket string) *S3Archive {
	return &S3Archive{client: client, bucket: bucket}
}

func (a *S3Archive) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(a.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(a.bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys, nil
}

func (a *S3Archive) Put(ctx context.Context, key string, body []byte) error {
	_, err := a.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(a.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	})
	return err
}
