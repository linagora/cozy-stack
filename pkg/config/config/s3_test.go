package config

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/s3util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestS3Configuration(t *testing.T) {
	previous := config
	t.Cleanup(func() { config = previous })
	for _, value := range []string{"", "true", "false"} {
		t.Run("auto_create_buckets="+value, func(t *testing.T) {
			v := createTestViper()
			v.SetConfigType("yaml")
			setting := ""
			if value != "" {
				setting = "    auto_create_buckets: " + value + "\n"
			}
			err := v.ReadConfig(strings.NewReader("fs:\n  s3:\n" + setting + `    buckets:
      default:
        name: company-storage
      apps_web:
        name: company-web
        url: s3://apps.example.net?access_key=APPS&secret_key=SECRET
`))
			require.NoError(t, err)
			require.NoError(t, UseViper(v))
			s3 := config.Fs.S3
			assert.Equal(t, value != "false", s3.AutoCreateBuckets == nil || *s3.AutoCreateBuckets)
			assert.Equal(t, "s3://apps.example.net?access_key=APPS&secret_key=SECRET", s3.Buckets["apps_web"].URL)
			assert.Equal(t, "company-web", s3.Buckets["apps_web"].Name)
			assert.Equal(t, "company-storage", s3.Buckets["default"].Name)
		})
	}
	for _, key := range []string{"fs.url", "fs.s3.auto_create_buckets", "fs.s3.buckets.files", "fs.s3.buckets.files.urll"} {
		t.Run("invalid "+key, func(t *testing.T) {
			v := createTestViper()
			v.Set(key, "s3://bad%host?access_key=ACCESS_KEY&secret_key=SECRET_KEY")
			err := UseViper(v)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "ACCESS_KEY")
			assert.NotContains(t, err.Error(), "SECRET_KEY")
		})
	}
}

func TestS3EnvironmentOverride(t *testing.T) {
	previous := config
	t.Cleanup(func() { config = previous })
	for _, setting := range []string{"", "true", "false"} {
		for _, env := range []string{"true", "false"} {
			t.Run("yaml="+setting+"/env="+env, func(t *testing.T) {
				t.Setenv("COZY_FS_S3_AUTO_CREATE_BUCKETS", env)
				v := createTestViper()
				if setting != "" {
					v.SetConfigType("yaml")
					require.NoError(t, v.ReadConfig(strings.NewReader("fs:\n  s3:\n    auto_create_buckets: "+setting)))
				}
				require.NoError(t, UseViper(v))
				require.NotNil(t, config.Fs.S3.AutoCreateBuckets)
				assert.Equal(t, env == "true", *config.Fs.S3.AutoCreateBuckets)
			})
		}
	}
	t.Run("invalid environment value", func(t *testing.T) {
		t.Setenv("COZY_FS_S3_AUTO_CREATE_BUCKETS", "not-a-boolean")
		require.EqualError(t, UseViper(createTestViper()), "s3: invalid fs.s3.auto_create_buckets configuration")
	})
}

func TestS3BucketEnvironment(t *testing.T) {
	previous := config
	t.Cleanup(func() { config = previous })
	v := createTestViper()
	v.SetConfigType("yaml")
	require.NoError(t, v.ReadConfig(strings.NewReader("fs:\n  s3:\n    buckets:\n      default:\n        name: yaml-bucket\n      apps_web:\n        name: yaml-apps\n        url: s3://yaml.example")))
	t.Setenv("COZY_FS_S3_BUCKETS_DEFAULT_NAME", "env-bucket")
	t.Setenv("COZY_FS_S3_BUCKETS_APPS_WEB_NAME", "env-apps")
	t.Setenv("COZY_FS_S3_BUCKETS_APPS_WEB_URL", "s3://env.example")
	t.Setenv("COZY_FS_S3_BUCKETS_FILES_NAME", "env-files")
	require.NoError(t, UseViper(v))
	assert.Equal(t, "env-bucket", config.Fs.S3.Buckets["default"].Name)
	assert.Equal(t, FsS3Bucket{Name: "env-apps", URL: "s3://env.example"}, config.Fs.S3.Buckets["apps_web"])
	assert.Equal(t, "env-files", config.Fs.S3.Buckets["files"].Name)
}

func TestS3Connections(t *testing.T) {
	previous := s3Storages
	t.Cleanup(func() { s3Storages = previous })
	enabled, disabled := true, false
	for _, autoCreate := range []*bool{nil, &enabled, &disabled} {
		name := "default"
		if autoCreate != nil {
			name = fmt.Sprint(*autoCreate)
		}
		for _, layout := range []string{"shared", "separate", "mixed"} {
			t.Run(name+"/"+layout, func(t *testing.T) {
				fs := Fs{URL: s3TestURL(t, "default.example", "DEFAULT", "eu-west-3"), S3: FsS3{
					AutoCreateBuckets: autoCreate,
					Buckets:           map[string]FsS3Bucket{"default": {Name: "company-storage"}},
				}}
				if layout == "separate" {
					delete(fs.S3.Buckets, "default")
					for _, kind := range s3StorageKinds {
						fs.S3.Buckets[kind] = FsS3Bucket{Name: "company-" + strings.ReplaceAll(kind, "_", "-")}
					}
				}
				if layout != "shared" {
					fs.S3.Buckets["apps_web"] = FsS3Bucket{Name: "company-web", URL: s3TestURL(t, "apps.example", "APPS", "eu-west-1").String()}
					fs.S3.Buckets["previews"] = FsS3Bucket{Name: "company-cache", URL: s3TestURL(t, "cache.example", "CACHE", "us-east-1").String()}
				}
				requests := make(map[string]int)
				fs.Transport = s3TransportFunc(func(r *http.Request) (*http.Response, error) {
					bucket := strings.Trim(r.URL.Path, "/")
					requests[bucket]++
					method := http.MethodHead
					if autoCreate == nil || *autoCreate {
						method = http.MethodPut
					}
					assert.Equal(t, method, r.Method)
					assert.NotEmpty(t, bucket, "bucket listing is unnecessary")
					key, region := "DEFAULT", "eu-west-3"
					if bucket == "company-web" {
						key, region = "APPS", "eu-west-1"
					}
					if bucket == "company-cache" {
						key, region = "CACHE", "us-east-1"
					}
					assert.Contains(t, r.Header.Get("Authorization"), "Credential="+key+"/")
					assert.Contains(t, r.Header.Get("Authorization"), "/"+region+"/s3/aws4_request")
					deadline, ok := r.Context().Deadline()
					assert.True(t, ok)
					assert.WithinDuration(t, time.Now().Add(30*time.Second), deadline, time.Second)
					if method == http.MethodPut {
						if region == "us-east-1" {
							assert.Nil(t, r.Body)
						} else {
							body, err := io.ReadAll(r.Body)
							assert.NoError(t, err)
							var location struct {
								Region string `xml:"LocationConstraint"`
							}
							assert.NoError(t, xml.Unmarshal(body, &location))
							assert.Equal(t, region, location.Region)
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
				})
				require.NoError(t, InitS3Connection(fs))
				for _, kind := range s3StorageKinds {
					storage := GetS3Storage(kind)
					bucket := fs.S3.Buckets[kind].Name
					if bucket == "" {
						bucket = fs.S3.Buckets["default"].Name
					}
					assert.Equal(t, bucket, storage.Bucket)
					assert.Equal(t, strings.ReplaceAll(kind, "_", "-")+"/", storage.Prefix)
					assert.Equal(t, 1, requests[bucket], "initialize each physical bucket only once")
				}
				if layout == "shared" {
					assert.Len(t, requests, 1)
					assert.Same(t, GetS3Storage("files").Client, GetS3Storage("apps_web").Client)
				} else {
					assert.NotSame(t, GetS3Storage("files").Client, GetS3Storage("apps_web").Client)
				}
			})
		}
	}
}

func TestS3SharedConnections(t *testing.T) {
	previous := s3Storages
	t.Cleanup(func() { s3Storages = previous })
	disabled := false
	primary := s3TestURL(t, "primary.example", "PRIMARY", "us-east-1").String()
	secondary := s3TestURL(t, "secondary.example", "SECONDARY", "eu-west-1").String()
	requests := make(map[string]int)
	require.NoError(t, InitS3Connection(Fs{
		URL: s3TestURL(t, "unused.example", "DEFAULT", "us-east-1"),
		S3: FsS3{AutoCreateBuckets: &disabled, Buckets: map[string]FsS3Bucket{
			"default": {Name: "shared-bucket", URL: primary},
			"files":   {Name: "shared-bucket", URL: primary},
			"exports": {Name: "shared-bucket", URL: secondary},
		}},
		Transport: s3TransportFunc(func(r *http.Request) (*http.Response, error) {
			assert.Equal(t, http.MethodHead, r.Method)
			requests[r.URL.Host]++
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
		}),
	}))
	assert.Equal(t, map[string]int{"primary.example": 1, "secondary.example": 1}, requests)
	assert.Same(t, GetS3Storage("files").Client, GetS3Storage("assets").Client)
	assert.NotSame(t, GetS3Storage("files").Client, GetS3Storage("exports").Client)
}

func TestS3InvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		s3       FsS3
		expected string
	}{
		{"missing", FsS3{}, "missing bucket"},
		{"partial", FsS3{Buckets: map[string]FsS3Bucket{"files": {Name: "files-bucket"}}}, "apps_web"},
		{"empty override", FsS3{Buckets: map[string]FsS3Bucket{"default": {Name: "storage"}, "files": {}}}, "invalid bucket name"},
		{"invalid default", FsS3{Buckets: map[string]FsS3Bucket{"default": {Name: "Bad_bucket"}}}, "invalid bucket name"},
		{"invalid override", FsS3{Buckets: map[string]FsS3Bucket{"default": {Name: "storage"}, "files": {Name: "Bad_bucket"}}}, "invalid bucket name"},
		{"unknown type", FsS3{Buckets: map[string]FsS3Bucket{"default": {Name: "storage"}, "appsweb": {Name: "web-bucket"}}}, "unknown storage type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := Fs{URL: s3TestURL(t, "storage.example", "KEY", "us-east-1"), S3: tc.s3,
				Transport: s3TransportFunc(func(r *http.Request) (*http.Response, error) {
					t.Error("invalid configuration must not contact S3")
					return nil, errors.New("unexpected S3 request")
				})}
			require.ErrorContains(t, InitS3Connection(fs), tc.expected)
		})
	}
}

func TestS3ConnectionErrors(t *testing.T) {
	disabled := false
	for _, tc := range []struct {
		name, override, expected string
		status                   int
		transportError           bool
	}{
		{name: "missing", status: 404, expected: "does not exist"},
		{name: "forbidden", status: 403, expected: "inaccessible"},
		{name: "transport", transportError: true, expected: "inaccessible"},
		{name: "empty credentials", override: "s3://storage.example?region=us-east-1", expected: "no usable credentials"},
		{name: "incomplete credentials", override: "s3://storage.example?access_key=ACCESS_KEY&region=us-east-1", expected: "no usable credentials"},
		{name: "malformed URL", override: "s3://bad%host?access_key=ACCESS_KEY&secret_key=SECRET_KEY", expected: "invalid connection URL"},
		{name: "malformed query", override: "s3://storage.example?access_key=ACCESS_KEY&secret_key=SECRET_KEY%", expected: "invalid connection URL"},
		{name: "wrong scheme", override: "https://storage.example?access_key=ACCESS_KEY&secret_key=SECRET_KEY", expected: "invalid connection URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := Fs{URL: s3TestURL(t, "storage.example", "ACCESS_KEY", "us-east-1"),
				S3: FsS3{Buckets: map[string]FsS3Bucket{"default": {Name: "company-storage", URL: tc.override}}, AutoCreateBuckets: &disabled}}
			fs.Transport = s3TransportFunc(func(r *http.Request) (*http.Response, error) {
				assert.Equal(t, http.MethodHead, r.Method)
				if tc.transportError {
					return nil, errors.New("ACCESS_KEY SECRET_KEY")
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ACCESS_KEY SECRET_KEY")), Request: r}, nil
			})
			err := InitS3Connection(fs)
			require.ErrorContains(t, err, "company-storage")
			assert.ErrorContains(t, err, tc.expected)
			assert.NotContains(t, err.Error(), "ACCESS_KEY")
			assert.NotContains(t, err.Error(), "SECRET_KEY")
		})
	}
}

func TestS3BucketCheckDeadline(t *testing.T) {
	var deadline time.Time
	client, err := newS3Client(s3TestURL(t, "storage.example", "KEY", "us-east-1"), s3TransportFunc(func(r *http.Request) (*http.Response, error) {
		var ok bool
		deadline, ok = r.Context().Deadline()
		assert.True(t, ok)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
	}))
	require.NoError(t, err)
	require.NoError(t, s3util.CheckBucket(context.Background(), client, "company-storage"))
	assert.WithinDuration(t, time.Now().Add(30*time.Second), deadline, time.Second)
	parentDeadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), parentDeadline)
	defer cancel()
	require.NoError(t, s3util.CheckBucket(ctx, client, "company-storage"))
	assert.Equal(t, parentDeadline, deadline)
	cancel()
	require.Error(t, s3util.CheckBucket(ctx, client, "company-storage"))
}

func s3TestURL(t *testing.T, endpoint, key, region string) *url.URL {
	t.Helper()
	u, err := url.Parse("s3://" + endpoint)
	require.NoError(t, err)
	u.RawQuery = url.Values{"access_key": {key}, "secret_key": {key + "_SECRET"}, "region": {region}}.Encode()
	return u
}

type s3TransportFunc func(*http.Request) (*http.Response, error)

func (f s3TransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
