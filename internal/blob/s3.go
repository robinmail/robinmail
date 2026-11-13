package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3Config describes a bucket in S3-compatible storage (Garage, MinIO, R2,
// AWS). Requests are path-style: <Endpoint>/<Bucket>/<key>.
type S3Config struct {
	Endpoint  string // "https://s3.example.test"
	Region    string // "garage" for Garage, "auto" for R2, "us-east-1" for MinIO
	Bucket    string
	AccessKey string
	SecretKey string
	// Client is optional. The default has a 60 second timeout.
	Client *http.Client
}

// S3 is the S3 backend. It signs requests with AWS Signature Version 4.
type S3 struct {
	cfg  S3Config
	base *url.URL
	now  func() time.Time
}

func NewS3(cfg S3Config) (*S3, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("blob: s3 endpoint: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Trim(u.Path, "/") != "" {
		return nil, fmt.Errorf("blob: s3 endpoint %q must be http(s)://host[:port]", cfg.Endpoint)
	}
	if cfg.Region == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("blob: s3 needs region, bucket, access key and secret key")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 60 * time.Second}
	}
	return &S3{cfg: cfg, base: u, now: time.Now}, nil
}

func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	if err := checkKey(key); err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodPut, key, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError("put", key, resp)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	resp, err := s.do(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, ErrNotFound
	}
	defer resp.Body.Close()
	return nil, statusError("get", key, resp)
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	}
	return statusError("delete", key, resp)
}

// CreateBucket creates the configured bucket. Only the tests and development
// setups use it; a bucket that exists already is not an error.
func (s *S3) CreateBucket(ctx context.Context) error {
	resp, err := s.do(ctx, http.MethodPut, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		return statusError("create bucket", s.cfg.Bucket, resp)
	}
	return nil
}

func (s *S3) do(ctx context.Context, method, key string, body []byte) (*http.Response, error) {
	u := *s.base
	u.Path = "/" + s.cfg.Bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawPath = uriEncode(u.Path, false)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("blob: s3: %w", err)
	}
	sum := sha256.Sum256(body)
	sign(req, hex.EncodeToString(sum[:]), s.cfg.Region, s.cfg.AccessKey, s.cfg.SecretKey, s.now())
	resp, err := s.cfg.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: s3 %s %s: %w", strings.ToLower(method), key, err)
	}
	return resp, nil
}

func statusError(op, key string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("blob: s3 %s %s: HTTP %d: %s", op, key, resp.StatusCode,
		strings.ToValidUTF8(strings.TrimSpace(string(b)), ""))
}

// sign adds the AWS Signature Version 4 headers for service "s3" to req. It
// signs the host and every header already set on the request. payloadHash is
// the hex SHA-256 of the body.
func sign(req *http.Request, payloadHash, region, accessKey, secretKey string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, values := range req.Header {
		headers[strings.ToLower(name)] = strings.Join(strings.Fields(strings.Join(values, ",")), " ")
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, name := range names {
		canonHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signed := strings.Join(names, ";")

	query := req.URL.Query()
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonQuery []string
	for _, k := range keys {
		values := query[k]
		sort.Strings(values)
		for _, v := range values {
			canonQuery = append(canonQuery, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}

	canonical := strings.Join([]string{
		req.Method,
		uriEncode(req.URL.Path, false),
		strings.Join(canonQuery, "&"),
		canonHeaders.String(),
		signed,
		payloadHash,
	}, "\n")
	scope := date + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key := hmacSHA256([]byte("AWS4"+secretKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+signature)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// uriEncode percent-encodes everything but the unreserved characters, as
// SigV4 prescribes. A path keeps its slashes.
func uriEncode(s string, encodeSlash bool) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&15])
		}
	}
	return b.String()
}
