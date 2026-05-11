package zooid

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"path/filepath"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/khatru/blossom"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gosimple/slug"
	"github.com/spf13/afero"
)

type BlossomStore struct {
	Config *Config
	Events eventstore.Store
}

func loadAWSConfigForBlossomS3(ctx context.Context, s *BlossomS3Settings) (aws.Config, error) {
	return awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(s.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(s.AccessKey, s.SecretKey, "")),
	)
}

func s3APIClientForBlossomSettings(awsCfg aws.Config, s *BlossomS3Settings) *s3.Client {
	customEndpoint := s.Endpoint != ""
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if customEndpoint {
			o.BaseEndpoint = aws.String(s.Endpoint)
			// Custom endpoints (e.g. MinIO) expect path-style addressing.
			o.UsePathStyle = true
		}
	})
}

func blossomS3ObjectKey(slugName, sha256, keyPrefix string) string {
	rel := slugName + "/" + sha256
	if keyPrefix != "" {
		return keyPrefix + "/" + rel
	}
	return rel
}

func attachBlossomLocalBlobs(bs *blossom.BlossomServer, slugName string) {
	dir := filepath.Join(Env("MEDIA"), slugName)
	osfs := afero.NewOsFs()
	_ = osfs.MkdirAll(dir, 0755)

	bs.StoreBlob = func(ctx context.Context, sha256 string, ext string, body []byte) error {
		file, err := osfs.Create(filepath.Join(dir, sha256))
		if err != nil {
			return err
		}

		if _, err := io.Copy(file, bytes.NewReader(body)); err != nil {
			return err
		}

		return nil
	}

	bs.LoadBlob = func(ctx context.Context, sha256 string, ext string) (io.ReadSeeker, *url.URL, error) {
		file, err := osfs.Open(filepath.Join(dir, sha256))
		if err != nil {
			return nil, nil, err
		}
		return file, nil, nil
	}

	bs.DeleteBlob = func(ctx context.Context, sha256 string, ext string) error {
		return osfs.Remove(filepath.Join(dir, sha256))
	}
}

func attachBlossomS3Blobs(bs *blossom.BlossomServer, cfg *Config, slugName string) error {
	s := &cfg.Blossom.S3
	ctx := context.Background()

	awsCfg, err := loadAWSConfigForBlossomS3(ctx, s)
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}

	client := s3APIClientForBlossomSettings(awsCfg, s)
	bucket := s.Bucket

	bs.StoreBlob = func(ctx context.Context, sha256 string, ext string, body []byte) error {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(blossomS3ObjectKey(slugName, sha256, s.KeyPrefix)),
			Body:   bytes.NewReader(body),
		})
		return err
	}

	bs.LoadBlob = func(ctx context.Context, sha256 string, ext string) (io.ReadSeeker, *url.URL, error) {
		out, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(blossomS3ObjectKey(slugName, sha256, s.KeyPrefix)),
		})
		if err != nil {
			return nil, nil, err
		}
		defer out.Body.Close()

		data, err := io.ReadAll(out.Body)
		if err != nil {
			return nil, nil, err
		}

		return bytes.NewReader(data), nil, nil
	}

	bs.DeleteBlob = func(ctx context.Context, sha256 string, ext string) error {
		_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(blossomS3ObjectKey(slugName, sha256, s.KeyPrefix)),
		})
		return err
	}

	return nil
}

func (bl *BlossomStore) Enable(instance *Instance) {
	slugName := slug.Make(bl.Config.Schema)
	backend := blossom.New(instance.Relay, "https://"+bl.Config.Host)

	backend.Store = blossom.EventStoreBlobIndexWrapper{
		Store:      bl.Events,
		ServiceURL: "https://" + bl.Config.Host,
	}

	switch bl.Config.Blossom.Backend {
	case "local":
		attachBlossomLocalBlobs(backend, slugName)
	case "s3":
		if err := attachBlossomS3Blobs(backend, bl.Config, slugName); err != nil {
			log.Fatalf("blossom: s3: %v", err)
		}
	default:
		log.Fatalf("blossom: unknown backend %q (use local or s3)", bl.Config.Blossom.Backend)
	}

	backend.RejectUpload = func(ctx context.Context, auth *nostr.Event, size int, ext string) (bool, string, int) {
		if size > 10*1024*1024 {
			return true, "file too large", 413
		}

		if auth == nil || !instance.Management.IsMember(auth.PubKey) {
			return true, "unauthorized", 403
		}

		return false, ext, size
	}

	backend.RejectGet = func(ctx context.Context, auth *nostr.Event, sha256 string, ext string) (bool, string, int) {
		if !bl.Config.Blossom.AuthenticatedRead {
			return false, "", 200
		}

		if auth == nil || !instance.Management.IsMember(auth.PubKey) {
			return true, "unauthorized", 403
		}

		return false, "", 200
	}

	backend.RejectList = func(ctx context.Context, auth *nostr.Event, pubkey nostr.PubKey) (bool, string, int) {
		if auth == nil || !instance.Management.IsMember(auth.PubKey) {
			return true, "unauthorized", 403
		}

		return false, "", 200
	}

	backend.RejectDelete = func(ctx context.Context, auth *nostr.Event, sha256 string, ext string) (bool, string, int) {
		if auth == nil || !instance.Management.IsMember(auth.PubKey) {
			return true, "unauthorized", 403
		}

		return false, "", 200
	}

	instance.Relay.Info.SupportedNIPs = append(instance.Relay.Info.SupportedNIPs, "BUD-00")
	instance.Relay.Info.SupportedNIPs = append(instance.Relay.Info.SupportedNIPs, "BUD-01")
	instance.Relay.Info.SupportedNIPs = append(instance.Relay.Info.SupportedNIPs, "BUD-02")
	instance.Relay.Info.SupportedNIPs = append(instance.Relay.Info.SupportedNIPs, "BUD-11")
}
