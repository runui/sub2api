// Package falvideo adapts the fal queue API for video generation.
package falvideo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	StatusInQueue    = "IN_QUEUE"
	StatusInProgress = "IN_PROGRESS"
	StatusCompleted  = "COMPLETED"

	queueHost        = "queue.fal.run"
	maxResponseBytes = 1 << 20
)

var pathSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Client sends individual queue requests. It does not poll, retry, or download media.
type Client struct {
	do func(*http.Request) (*http.Response, error)
}

// NewClient accepts a single-request HTTP function, such as a RoundTripper's
// RoundTrip method. It MUST NOT follow redirects, even to the same host. Do
// not pass an http.Client.Do unless its CheckRedirect prevents all redirects.
// A nil function uses a default client with redirects disabled.
func NewClient(do func(*http.Request) (*http.Response, error)) *Client {
	if do == nil {
		do = (&http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}).Do
	}
	return &Client{do: do}
}

// Input is the Grok video input sent to fal. Optional zero values are omitted.
// Duration is an integer number of seconds.
type Input struct {
	Prompt      string `json:"prompt"`
	ImageURL    string `json:"image_url,omitempty"`
	Duration    int    `json:"duration,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
}

// Reference is the persistable queue handle returned by Submit.
type Reference struct {
	RequestID   string `json:"request_id"`
	StatusURL   string `json:"status_url"`
	ResponseURL string `json:"response_url"`
}

// Result contains a video URL and its metadata, which may be absent in fal's response.
type Result struct {
	URL      string  `json:"url"`
	Duration float64 `json:"duration"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
}

// Error represents a non-success HTTP response from fal. Code and Message are
// optional provider data and should not be exposed to end users verbatim.
type Error struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("falvideo: upstream HTTP status %d", e.StatusCode)
}

// Submit enqueues a model request. endpoint is an owner/app[/variant...] ID or
// its absolute queue URL. Follow-up URLs may use the full endpoint or owner/app.
func (c *Client) Submit(ctx context.Context, key, endpoint string, input Input) (Reference, error) {
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + queueHost + "/" + endpoint
	}
	u, parts, err := queueURL(endpoint)
	if err != nil || !isModelPath(parts) {
		return Reference{}, errors.New("falvideo: invalid model endpoint")
	}
	if input.Duration < 0 {
		return Reference{}, errors.New("falvideo: duration must not be negative")
	}
	if input.ImageURL != "" && !isHTTPSURL(input.ImageURL) {
		return Reference{}, errors.New("falvideo: invalid image URL")
	}

	payload, err := json.Marshal(input)
	if err != nil {
		return Reference{}, fmt.Errorf("falvideo: encode input: %w", err)
	}
	var ref Reference
	if err := c.request(ctx, key, http.MethodPost, u, payload, &ref); err != nil {
		return Reference{}, err
	}
	_, resultURL, err := ref.urls()
	if err != nil {
		return Reference{}, err
	}
	modelPath := strings.TrimSuffix(resultURL.Path, "/requests/"+ref.RequestID)
	if modelPath != u.Path && modelPath != "/"+strings.Join(parts[:2], "/") {
		return Reference{}, errors.New("falvideo: queue reference does not match submitted model")
	}
	return ref, nil
}

// Status returns IN_QUEUE, IN_PROGRESS, or COMPLETED. COMPLETED is terminal,
// not necessarily successful; call Result to retrieve the output or model error.
func (c *Client) Status(ctx context.Context, key string, ref Reference) (string, error) {
	u, _, err := ref.urls()
	if err != nil {
		return "", err
	}
	var payload struct {
		Status    string `json:"status"`
		RequestID string `json:"request_id"`
	}
	if err := c.request(ctx, key, http.MethodGet, u, nil, &payload); err != nil {
		return "", err
	}
	if payload.RequestID != ref.RequestID {
		return "", errors.New("falvideo: status response request ID does not match reference")
	}
	switch payload.Status {
	case StatusInQueue, StatusInProgress, StatusCompleted:
		return payload.Status, nil
	default:
		return "", errors.New("falvideo: unknown queue status")
	}
}

// Result retrieves the completed model response. The video URL is returned as
// data only; it is never requested by this client.
func (c *Client) Result(ctx context.Context, key string, ref Reference) (Result, error) {
	_, u, err := ref.urls()
	if err != nil {
		return Result{}, err
	}
	var payload struct {
		Video Result `json:"video"`
	}
	if err := c.request(ctx, key, http.MethodGet, u, nil, &payload); err != nil {
		return Result{}, err
	}
	if !isHTTPSURL(payload.Video.URL) {
		return Result{}, errors.New("falvideo: result has no valid video URL")
	}
	if payload.Video.Duration < 0 || payload.Video.Width < 0 || payload.Video.Height < 0 {
		return Result{}, errors.New("falvideo: invalid video metadata")
	}
	return payload.Video, nil
}

func isHTTPSURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil &&
		!strings.ContainsAny(raw, " \\#")
}

func queueURL(raw string) (*url.URL, []string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" ||
		u.ForceQuery || u.RawQuery != "" || strings.Contains(raw, "#") ||
		(u.Host != queueHost && u.Host != "queue.falrun.com") || u.EscapedPath() != u.Path {
		return nil, nil, errors.New("falvideo: invalid queue URL")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	for _, part := range parts {
		if !pathSegment.MatchString(part) {
			return nil, nil, errors.New("falvideo: invalid queue URL path")
		}
	}
	return u, parts, nil
}

func isModelPath(parts []string) bool {
	return len(parts) >= 2 && !slices.Contains(parts[2:], "requests")
}

func (r Reference) urls() (statusURL, responseURL *url.URL, err error) {
	status, _, err := queueURL(r.StatusURL)
	if err != nil {
		return nil, nil, err
	}
	response, responseParts, err := queueURL(r.ResponseURL)
	if err != nil {
		return nil, nil, err
	}
	n := len(responseParts)
	if n < 4 || !isModelPath(responseParts[:n-2]) || responseParts[n-2] != "requests" ||
		responseParts[n-1] != r.RequestID || status.Path != response.Path+"/status" {
		return nil, nil, errors.New("falvideo: inconsistent queue reference")
	}
	return status, response, nil
}

func (c *Client) request(ctx context.Context, key, method string, u *url.URL, payload []byte, dst any) error {
	if key == "" || strings.ContainsFunc(key, func(r rune) bool { return r <= ' ' || r >= 127 }) {
		return errors.New("falvideo: invalid API key")
	}
	if c == nil || c.do == nil {
		return errors.New("falvideo: uninitialized client")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("falvideo: create request: %w", err)
	}
	req.Header.Set("Authorization", "Key "+key)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("falvideo: send request: %w", err)
	}
	if resp == nil {
		return errors.New("falvideo: missing HTTP response")
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	isJSON := mediaType == "application/json" || (strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json"))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		upstream := &Error{StatusCode: resp.StatusCode}
		if resp.StatusCode >= 400 && isJSON && resp.Body != nil {
			var details struct {
				Code      string `json:"type"`
				ErrorType string `json:"error_type"`
				Detail    any    `json:"detail"`
				Error     string `json:"error"`
			}
			if readJSON(resp.Body, &details) == nil {
				upstream.Code = details.ErrorType
				if upstream.Code == "" {
					upstream.Code = details.Code
				}
				upstream.Message = details.Error
				if upstream.Message == "" {
					upstream.Message, _ = details.Detail.(string)
				}
			}
		}
		return upstream
	}
	if !isJSON || resp.Body == nil {
		return errors.New("falvideo: expected a JSON response")
	}
	return readJSON(resp.Body, dst)
}

func readJSON(r io.Reader, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("falvideo: read response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return errors.New("falvideo: response exceeds size limit")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("falvideo: decode response: %w", err)
	}
	return nil
}
