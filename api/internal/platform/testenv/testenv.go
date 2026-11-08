// Package testenv gives integration tests access to the real services they need (Postgres,
// Redis, object storage) from TEST_* environment variables. Locally a missing variable skips
// the test; in CI it fails it, so a misconfigured pipeline can never turn into a green skip.
package testenv

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"enclave/api/internal/platform/objectstore"
	"enclave/api/internal/platform/postgres"
	"enclave/api/internal/platform/redis"
)

// Require returns the value of each variable, skipping or failing the test if any is unset.
func Require(t *testing.T, keys ...string) []string {
	t.Helper()
	values := make([]string, len(keys))
	var missing []string
	for i, key := range keys {
		values[i] = os.Getenv(key)
		if values[i] == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		message := strings.Join(missing, ", ") + " not set; run `make test`"
		if os.Getenv("CI") != "" {
			t.Fatal(message + " (must be set in CI)")
		}
		t.Skip(message)
	}
	return values
}

func Postgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := Require(t, "TEST_DATABASE_URL")[0]
	pool, err := postgres.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func Redis(t *testing.T) *goredis.Client {
	t.Helper()
	url := Require(t, "TEST_REDIS_URL")[0]
	client, err := redis.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connect to test redis: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// ObjectStore returns a store on a bucket of its own (created if needed), so tests never see
// each other's files.
func ObjectStore(t *testing.T, bucket string) *objectstore.Store {
	t.Helper()
	values := Require(t, "TEST_OBJECTSTORE_ENDPOINT", "TEST_OBJECTSTORE_ACCESS_KEY", "TEST_OBJECTSTORE_SECRET_KEY")
	store := objectstore.New(objectstore.Config{
		Endpoint:  values[0],
		Region:    "us-east-1",
		Bucket:    bucket,
		AccessKey: values[1],
		SecretKey: values[2],
	})
	if err := store.EnsureBucket(context.Background()); err != nil {
		t.Fatalf("create test bucket: %v", err)
	}
	return store
}
