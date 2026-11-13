package modelproxy

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// awsCreds is one AWS credential, temporary when Expires is set.
type awsCreds struct {
	AccessKeyID, SecretAccessKey, SessionToken string
	Expires                                    time.Time
}

type credSource func(ctx context.Context) (awsCreds, error)

// awsCredentials finds the credential the chart's three aws.auth modes give
// the proxy, in the AWS SDKs' order: static keys (access_keys), IRSA's web
// identity, then the container credential endpoint (Pod Identity). No
// profiles, config files or instance metadata: nothing in a job pod sets
// them, and the node's role isn't devbot's.
func awsCredentials(getenv func(string) string, region string) (credSource, error) {
	hc := &http.Client{Timeout: 30 * time.Second}
	if id, secret := getenv("AWS_ACCESS_KEY_ID"), getenv("AWS_SECRET_ACCESS_KEY"); id != "" && secret != "" {
		c := awsCreds{AccessKeyID: id, SecretAccessKey: secret, SessionToken: getenv("AWS_SESSION_TOKEN")}
		return func(context.Context) (awsCreds, error) { return c, nil }, nil
	}
	if file, role := getenv("AWS_WEB_IDENTITY_TOKEN_FILE"), getenv("AWS_ROLE_ARN"); file != "" && role != "" {
		stsRegion := cmp.Or(getenv("AWS_REGION"), getenv("AWS_DEFAULT_REGION"), region)
		endpoint := cmp.Or(getenv("AWS_ENDPOINT_URL_STS"), "https://sts."+stsRegion+".amazonaws.com")
		session := cmp.Or(getenv("AWS_ROLE_SESSION_NAME"), "devbot-model-proxy")
		return cached(func(ctx context.Context) (awsCreds, error) {
			return webIdentity(ctx, hc, endpoint, role, session, file)
		}), nil
	}
	uri := getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	if rel := getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); uri == "" && rel != "" {
		uri = "http://169.254.170.2" + rel
	}
	if uri != "" {
		tokenFile, token := getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"), getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")
		return cached(func(ctx context.Context) (awsCreds, error) {
			return container(ctx, hc, uri, tokenFile, token)
		}), nil
	}
	return nil, errors.New("model proxy: no AWS credential: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or run with IRSA or EKS Pod Identity")
}

// cached keeps a temporary credential until five minutes before it expires.
func cached(fetch credSource) credSource {
	var mu sync.Mutex
	var cur awsCreds
	return func(ctx context.Context) (awsCreds, error) {
		mu.Lock()
		defer mu.Unlock()
		if cur.AccessKeyID != "" && (cur.Expires.IsZero() || time.Until(cur.Expires) > 5*time.Minute) {
			return cur, nil
		}
		c, err := fetch(ctx)
		if err != nil {
			return awsCreds{}, fmt.Errorf("failed to load AWS credentials: %w", err)
		}
		cur = c
		return c, nil
	}
}

// webIdentity is STS's AssumeRoleWithWebIdentity with IRSA's projected
// token, read again each time since the kubelet rotates it.
func webIdentity(ctx context.Context, hc *http.Client, endpoint, role, session, tokenFile string) (awsCreds, error) {
	tok, err := os.ReadFile(tokenFile)
	if err != nil {
		return awsCreds{}, err
	}
	form := url.Values{"Action": {"AssumeRoleWithWebIdentity"}, "Version": {"2011-06-15"}, "RoleArn": {role},
		"RoleSessionName": {session}, "WebIdentityToken": {strings.TrimSpace(string(tok))}}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return awsCreds{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	body, err := do(hc, req)
	if err != nil {
		return awsCreds{}, fmt.Errorf("sts AssumeRoleWithWebIdentity: %w", err)
	}
	var out struct {
		Creds struct {
			AccessKeyID     string    `xml:"AccessKeyId"`
			SecretAccessKey string    `xml:"SecretAccessKey"`
			SessionToken    string    `xml:"SessionToken"`
			Expiration      time.Time `xml:"Expiration"`
		} `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
	}
	if err := xml.Unmarshal(body, &out); err != nil || out.Creds.AccessKeyID == "" {
		return awsCreds{}, fmt.Errorf("sts AssumeRoleWithWebIdentity: no credentials in the answer (%v)", err)
	}
	return awsCreds{out.Creds.AccessKeyID, out.Creds.SecretAccessKey, out.Creds.SessionToken, out.Creds.Expiration}, nil
}

// container is the container credential endpoint: EKS Pod Identity's agent
// on the node, or ECS's.
func container(ctx context.Context, hc *http.Client, uri, tokenFile, token string) (awsCreds, error) {
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return awsCreds{}, err
		}
		token = strings.TrimSpace(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, "GET", uri, nil)
	if err != nil {
		return awsCreds{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	body, err := do(hc, req)
	if err != nil {
		return awsCreds{}, fmt.Errorf("container credentials: %w", err)
	}
	var out struct {
		AccessKeyID     string    `json:"AccessKeyId"`
		SecretAccessKey string    `json:"SecretAccessKey"`
		Token           string    `json:"Token"`
		Expiration      time.Time `json:"Expiration"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessKeyID == "" {
		return awsCreds{}, fmt.Errorf("container credentials: no credentials in the answer (%v)", err)
	}
	return awsCreds{out.AccessKeyID, out.SecretAccessKey, out.Token, out.Expiration}, nil
}

func do(hc *http.Client, req *http.Request) ([]byte, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func sigV4(creds credSource, region, service string) func(context.Context, *http.Request, []byte) error {
	return func(ctx context.Context, r *http.Request, body []byte) error {
		c, err := creds(ctx)
		if err != nil {
			return err
		}
		sign(r, body, c, region, service, time.Now().UTC())
		return nil
	}
}

// sign adds AWS Signature Version 4 to r. The path is escaped the way the
// AWS SDKs send it (a model ID's ":" as %3A) and escaped again for the
// canonical request, as SigV4 has it for every service but S3.
func sign(r *http.Request, body []byte, c awsCreds, region, service string, now time.Time) {
	amzDate, day := now.Format("20060102T150405Z"), now.Format("20060102")
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Del("X-Amz-Security-Token")
	if c.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	r.URL.RawPath = escapePath(path)
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}

	headers := map[string]string{"host": host, "x-amz-date": amzDate}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		headers["content-type"] = ct
	}
	if c.SessionToken != "" {
		headers["x-amz-security-token"] = c.SessionToken
	}
	names := slices.Sorted(maps.Keys(headers))
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	payload := sha256.Sum256(body)
	canonical := strings.Join([]string{r.Method, escapePath(r.URL.RawPath), canonicalQuery(r.URL.RawQuery),
		canonHeaders.String(), signed, hex.EncodeToString(payload[:])}, "\n")

	scope := day + "/" + region + "/" + service + "/aws4_request"
	hash := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(hash[:])
	key := []byte("AWS4" + c.SecretAccessKey)
	for _, part := range []string{day, region, service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyID+"/"+scope+
		", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmacSHA256(key, toSign)))
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func canonicalQuery(raw string) string {
	q, _ := url.ParseQuery(raw)
	var pairs [][2]string
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, [2]string{awsEscape(k), awsEscape(v)})
		}
	}
	// By name, then value.
	slices.SortFunc(pairs, func(a, b [2]string) int { return cmp.Or(strings.Compare(a[0], b[0]), strings.Compare(a[1], b[1])) })
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p[0] + "=" + p[1]
	}
	return strings.Join(parts, "&")
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsEscape(s)
	}
	return strings.Join(segs, "/")
}

// awsEscape is SigV4's URI encoding: RFC 3986's unreserved characters as
// they are, every other byte as %XX.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
