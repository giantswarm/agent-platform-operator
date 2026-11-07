// Package snapshots deletes an organization's worker snapshots from the S3
// snapshot bucket.
//
// The client takes its credentials from the AWS SDK's default chain: on a
// management cluster, the IRSA role the agent-platform chart creates for the
// operator's ServiceAccount, whose policy allows deletes under the
// organizations' prefixes only.
package snapshots

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// timeout bounds one DeletePrefix.
const timeout = 5 * time.Minute

// S3 deletes snapshots from S3.
type S3 struct{ client *s3.Client }

// NewS3 is an S3 client with the default credential chain.
func NewS3(ctx context.Context, endpoint string, usePathStyle bool) (*S3, error) {
	configuration, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load the AWS configuration: %w", err)
	}
	return &S3{client: s3.NewFromConfig(configuration, func(options *s3.Options) {
		if endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
		}
		options.UsePathStyle = usePathStyle
	})}, nil
}

// DeletePrefix deletes every object under prefix, s3://<bucket>/<path>: the
// objects whose keys start with <path>/.
func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	// A bucket that stops answering never holds the reconciler.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bucket, keyPrefix, err := parse(prefix)
	if err != nil {
		return err
	}
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(keyPrefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		if len(page.Contents) == 0 {
			continue
		}
		objects := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, o := range page.Contents {
			objects = append(objects, types.ObjectIdentifier{Key: o.Key})
		}
		// A page holds at most 1000 keys, the most one DeleteObjects takes.
		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("delete under %s: %w", prefix, err)
		}
		if len(out.Errors) > 0 {
			errs := make([]error, 0, len(out.Errors))
			for _, e := range out.Errors {
				errs = append(errs, fmt.Errorf("delete s3://%s/%s: %s", bucket, aws.ToString(e.Key), aws.ToString(e.Message)))
			}
			return errors.Join(errs...)
		}
	}
	return nil
}

// parse splits s3://<bucket>/<path> into the bucket and the key prefix
// <path>/, so a prefix never matches a longer sibling's keys.
func parse(prefix string) (bucket, keyPrefix string, err error) {
	rest, ok := strings.CutPrefix(prefix, "s3://")
	bucket, path, ok2 := strings.Cut(rest, "/")
	path = strings.Trim(path, "/")
	if !ok || !ok2 || bucket == "" || path == "" {
		return "", "", fmt.Errorf("snapshot prefix %q is not s3://<bucket>/<path>", prefix)
	}
	return bucket, path + "/", nil
}
