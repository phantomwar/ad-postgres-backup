package s3sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Options configures an S3-compatible offsite backup target.
type Options struct {
	Endpoint          string
	Bucket            string
	Region            string
	Prefix            string
	AccessKeyID       string
	SecretAccessKey   string
	ForcePathStyle    bool
	RequireEncryption bool
}

// Client uploads encrypted backup files and enforces remote retention policies.
type Client struct {
	s3                *s3.Client
	bucket            string
	prefix            string
	requireEncryption bool
}

// NewClient initializes an S3 client compatible with Garage, MinIO, AWS S3, and Cloudflare R2.
func NewClient(ctx context.Context, opts Options) (*Client, error) {
	if opts.Bucket == "" {
		return nil, fmt.Errorf("s3sync: Bucket is required")
	}
	region := opts.Region
	if region == "" {
		region = "us-east-1"
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(opts.AccessKeyID, opts.SecretAccessKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("s3sync: load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = opts.ForcePathStyle
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
	})

	return &Client{
		s3:                client,
		bucket:            opts.Bucket,
		prefix:            strings.Trim(opts.Prefix, "/"),
		requireEncryption: opts.RequireEncryption,
	}, nil
}

// UploadBackup streams filePath (and its .sha256 sidecar if present) to S3.
// If RequireEncryption is enabled, plaintext files without ".age" are rejected (fail-closed).
func (c *Client) UploadBackup(ctx context.Context, filePath string) error {
	if c.requireEncryption && !strings.HasSuffix(filePath, ".age") {
		return fmt.Errorf("s3sync: refusing unencrypted upload %q when S3_REQUIRE_ENCRYPTION=true", filePath)
	}
	if err := c.putFile(ctx, filePath, "application/octet-stream"); err != nil {
		return err
	}
	sumPath := filePath + ".sha256"
	if _, err := os.Stat(sumPath); err == nil {
		if err := c.putFile(ctx, sumPath, "text/plain; charset=utf-8"); err != nil {
			return err
		}
	}
	return nil
}

// PruneExpired deletes objects under the configured prefix whose LastModified is older than retentionDays.
func (c *Client) PruneExpired(ctx context.Context, retentionDays int, now time.Time) (int, error) {
	if retentionDays <= 0 {
		return 0, fmt.Errorf("s3sync: retentionDays must be > 0 (got %d)", retentionDays)
	}
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)

	var listPrefix *string
	if c.prefix != "" {
		p := c.prefix + "/"
		listPrefix = aws.String(p)
	}

	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: listPrefix,
	})

	deleted := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return deleted, fmt.Errorf("s3sync: ListObjectsV2: %w", err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil || obj.LastModified == nil {
				continue
			}
			if obj.LastModified.Before(cutoff) {
				_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket: aws.String(c.bucket),
					Key:    obj.Key,
				})
				if err != nil {
					return deleted, fmt.Errorf("s3sync: DeleteObject(%q): %w", *obj.Key, err)
				}
				deleted++
			}
		}
	}
	return deleted, nil
}

func (c *Client) putFile(ctx context.Context, localPath, contentType string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("s3sync: open %q: %w", localPath, err)
	}
	defer f.Close()

	key := c.objectKey(filepath.Base(localPath))
	_, err = c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        f,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("s3sync: PutObject(%q): %w", key, err)
	}
	return nil
}

func (c *Client) objectKey(base string) string {
	if c.prefix == "" {
		return base
	}
	return c.prefix + "/" + base
}
