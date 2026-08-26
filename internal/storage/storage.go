// Package storage wraps S3-compatible object storage (AWS S3, MinIO, R2) for
// the bot's large blobs: raw session audio and generated art, kept out of
// Postgres so storage scales/persists independently.
package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	appcfg "github.com/stephencshelton/discord-dnd-bot/internal/config"
)

// Store is an object-storage client scoped to a single bucket.
type Store struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

// New builds a storage client from config. When Endpoint is set (MinIO/R2) it
// uses static credentials and path-style addressing; otherwise it falls back to
// the default AWS credential chain (IRSA, env, etc.).
func New(ctx context.Context, cfg appcfg.StorageConfig) (*Store, error) {
	var optFns []func(*awsconfig.LoadOptions) error
	optFns = append(optFns, awsconfig.WithRegion(cfg.Region))
	if cfg.AccessKeyID != "" {
		optFns = append(optFns, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})
	return &Store{
		client:   client,
		uploader: manager.NewUploader(client),
		bucket:   cfg.Bucket,
	}, nil
}

// Put uploads bytes under key and returns the key.
func (s *Store) Put(ctx context.Context, key, contentType string, r io.Reader) (string, error) {
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        r,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("put object %s: %w", key, err)
	}
	return key, nil
}

// Get downloads an object into memory. Suitable for audio/art (tens of MB).
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, out.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// List returns the keys of all objects under prefix, sorted lexicographically
// (S3 returns them sorted; checkpoint chunk keys are zero-padded so lexical
// order == chronological order). It pages through the full result set.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list objects %s: %w", prefix, err)
		}
		for _, obj := range out.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return keys, nil
}
