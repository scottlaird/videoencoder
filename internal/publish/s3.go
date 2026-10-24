package publish

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config describes an S3-compatible bucket.
type S3Config struct {
	// Endpoint is a URL such as "http://seaweed:8333". If empty, it is
	// taken from $S3_ENDPOINT_URL, $AWS_ENDPOINT_URL_S3 or
	// $AWS_ENDPOINT_URL.
	Endpoint string
	Bucket   string
	// Prefix is prepended to every object name, such as "videos/hike/".
	Prefix string
	Region string
	// Profile names a section of ~/.aws/credentials, used when the
	// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY variables aren't set.
	Profile string
}

// S3 is a bucket target.
type S3 struct {
	cfg    S3Config
	client *minio.Client
}

// EndpointFromEnv returns the S3 endpoint URL from the environment.
func EndpointFromEnv() string {
	for _, v := range []string{"S3_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL"} {
		if e := os.Getenv(v); e != "" {
			return e
		}
	}
	return ""
}

// NewS3 connects to the bucket in cfg. Credentials come from the
// environment or ~/.aws/credentials; they are never in config.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = EndpointFromEnv()
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("no S3 endpoint: set s3.endpoint or $S3_ENDPOINT_URL")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("bad S3 endpoint %q: want http(s)://host[:port]", cfg.Endpoint)
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("no S3 bucket")
	}
	creds := credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.FileAWSCredentials{Profile: cfg.Profile},
	})
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        creds,
		Secure:       u.Scheme == "https",
		Region:       cfg.Region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, err
	}
	return &S3{cfg: cfg, client: client}, nil
}

func (s *S3) String() string { return "s3://" + s.cfg.Bucket + "/" + s.cfg.Prefix }

func (s *S3) List(ctx context.Context) (map[string]Object, error) {
	out := map[string]Object{}
	for o := range s.client.ListObjects(ctx, s.cfg.Bucket, minio.ListObjectsOptions{Prefix: s.cfg.Prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, o.Err
		}
		etag := strings.Trim(o.ETag, `"`)
		if strings.Contains(etag, "-") || len(etag) != 32 {
			etag = "" // multipart: not an MD5
		}
		out[strings.TrimPrefix(o.Key, s.cfg.Prefix)] = Object{Size: o.Size, MD5: etag}
	}
	return out, nil
}

func (s *S3) Put(ctx context.Context, name, localPath string, size int64, meta Meta) error {
	_, err := s.client.FPutObject(ctx, s.cfg.Bucket, s.cfg.Prefix+name, localPath, minio.PutObjectOptions{
		ContentType:  meta.ContentType,
		CacheControl: meta.CacheControl,
		PartSize:     64 << 20,
	})
	return err
}

func (s *S3) Delete(ctx context.Context, name string) error {
	return s.client.RemoveObject(ctx, s.cfg.Bucket, s.cfg.Prefix+name, minio.RemoveObjectOptions{})
}
