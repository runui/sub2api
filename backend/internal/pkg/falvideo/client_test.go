package falvideo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const (
	requestID = "764cabcf-b745-4b3e-ae38-1200304cf45b"
	resultURL = "https://queue.fal.run/xai/grok-imagine-video/requests/" + requestID
)

func validReference() Reference {
	return Reference{RequestID: requestID, StatusURL: resultURL + "/status", ResponseURL: resultURL}
}

func response(code int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestQueueLifecycle(t *testing.T) {
	for _, path := range []string{"xai/grok-imagine-video", "xai/grok-imagine-video/v1.5/image-to-video"} {
		t.Run(path, func(t *testing.T) {
			testQueueLifecycle(t, path)
		})
	}
}

func testQueueLifecycle(t *testing.T, referencePath string) {
	t.Helper()
	const endpoint = "xai/grok-imagine-video/v1.5/image-to-video"
	ref := validReference()
	ref.ResponseURL = "https://" + queueHost + "/" + referencePath + "/requests/" + requestID
	ref.StatusURL = "https://queue.falrun.com/" + referencePath + "/requests/" + requestID + "/status"
	submitResponse, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	videoURL := "https://v3.fal.media/clip.mp4?token=abc"
	responses := []struct {
		method, url, body string
		code              int
	}{
		{http.MethodPost, "https://queue.fal.run/" + endpoint, string(submitResponse), http.StatusAccepted},
		{http.MethodGet, ref.StatusURL, `{"status":"IN_QUEUE","request_id":"` + requestID + `"}`, http.StatusAccepted},
		{http.MethodGet, ref.StatusURL, `{"status":"IN_PROGRESS","request_id":"` + requestID + `"}`, http.StatusAccepted},
		{http.MethodGet, ref.StatusURL, `{"status":"COMPLETED","request_id":"` + requestID + `","error":null}`, http.StatusOK},
		{http.MethodGet, ref.ResponseURL, `{"video":{"url":"` + videoURL + `","duration":5.5,"width":1280,"height":720}}`, http.StatusOK},
	}
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(func(req *http.Request) (*http.Response, error) {
		if calls == len(responses) {
			t.Fatal("unexpected request")
		}
		want := responses[calls]
		calls++
		if req.Method != want.method || req.URL.String() != want.url || req.Context() != ctx {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Key test-key" || req.Header.Get("Accept") != "application/json" {
			t.Fatalf("unexpected request headers: %v", req.Header)
		}
		if req.Method == http.MethodPost {
			if req.Header.Get("Content-Type") != "application/json" {
				t.Fatal("missing JSON content type")
			}
			var got map[string]any
			if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"prompt": "waves", "image_url": "https://example.com/frame.png", "duration": float64(5), "resolution": "720p", "aspect_ratio": "16:9"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("submit input = %v, want %v", got, want)
			}
		} else if req.Body != nil && req.Body != http.NoBody {
			t.Fatal("unexpected GET body")
		}
		return response(want.code, "application/json; charset=utf-8", want.body), nil
	})
	gotRef, err := client.Submit(ctx, "test-key", endpoint, Input{
		Prompt: "waves", ImageURL: "https://example.com/frame.png", Duration: 5, Resolution: "720p", AspectRatio: "16:9",
	})
	if err != nil || gotRef != ref {
		t.Fatalf("Submit = %+v, %v; want %+v", gotRef, err, ref)
	}
	for _, want := range []string{StatusInQueue, StatusInProgress, StatusCompleted} {
		got, err := client.Status(ctx, "test-key", gotRef)
		if err != nil || got != want {
			t.Fatalf("Status = %q, %v; want %q", got, err, want)
		}
	}
	got, err := client.Result(ctx, "test-key", gotRef)
	if want := (Result{URL: videoURL, Duration: 5.5, Width: 1280, Height: 720}); err != nil || got != want {
		t.Fatalf("Result = %+v, %v; want %+v", got, err, want)
	}
}

func TestSubmitOmitsUnsetInputFields(t *testing.T) {
	const endpoint = "https://queue.falrun.com/xai/grok-imagine-video/v1.5/text-to-video"
	client := NewClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != endpoint {
			t.Fatalf("submit URL = %s, want %s", req.URL, endpoint)
		}
		var got map[string]any
		if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if want := (map[string]any{"prompt": "waves"}); !reflect.DeepEqual(got, want) {
			t.Fatalf("submit input = %#v, want %#v", got, want)
		}
		body, err := json.Marshal(validReference())
		if err != nil {
			t.Fatal(err)
		}
		return response(http.StatusOK, "application/json", string(body)), nil
	})
	if _, err := client.Submit(context.Background(), "test-key", endpoint, Input{Prompt: "waves"}); err != nil {
		t.Fatal(err)
	}
}

func TestRejectInvalidSubmitEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"", "solo", "/xai/grok-imagine-video", "xai/../model", "xai//model", "xai/model/", "xai/model/%2e%2e",
		"xai/model?foo=bar", "xai/model#", "xai/model/requests/id", "xai/model/v1.5/requests/id", "xai/model\\other",
		"http://queue.fal.run/xai/model", "https://queue.fal.run:443/xai/model", "https://queue.fal.run./xai/model",
		"https://queue.fal.run.evil.example/xai/model", "https://queue.fal.run@evil.example/xai/model", "https://user@queue.fal.run/xai/model",
	} {
		t.Run(endpoint, func(t *testing.T) {
			client := noRequests(t)
			if _, err := client.Submit(context.Background(), "test-key", endpoint, Input{}); err == nil {
				t.Fatal("invalid endpoint accepted")
			}
		})
	}
}

func TestRejectInvalidInputBeforeSendingRequest(t *testing.T) {
	ctx := context.Background()
	client := noRequests(t)
	for _, tc := range []struct {
		key   string
		input Input
	}{
		{"", Input{Prompt: "waves"}},
		{"key\r\nheader", Input{Prompt: "waves"}},
		{"key", Input{Prompt: "waves", Duration: -1}},
		{"key", Input{Prompt: "waves", ImageURL: "https://user@example.com/frame.png"}},
	} {
		if _, err := client.Submit(ctx, tc.key, "xai/grok-imagine-video/image-to-video", tc.input); err == nil {
			t.Fatal("accepted invalid input")
		}
	}
}

func TestRejectInvalidReferenceBeforeSendingCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Reference)
	}{
		{"invalid ID", func(r *Reference) { r.RequestID = "other" }},
		{"empty ID", func(r *Reference) { r.RequestID = "" }},
		{"wrong status route", func(r *Reference) { r.StatusURL += "/stream" }},
		{"query", func(r *Reference) { r.StatusURL += "?logs=1" }},
		{"encoded separator", func(r *Reference) { r.StatusURL = strings.Replace(r.StatusURL, "/status", "%2Fstatus", 1) }},
		{"wrong app", func(r *Reference) { r.StatusURL = strings.Replace(r.StatusURL, "/xai/", "/other/", 1) }},
		{"mismatched variant", func(r *Reference) {
			r.StatusURL = strings.Replace(r.StatusURL, "/requests/", "/v1.5/text-to-video/requests/", 1)
			r.ResponseURL = strings.Replace(r.ResponseURL, "/requests/", "/v1.5/image-to-video/requests/", 1)
		}},
		{"nested request route", func(r *Reference) {
			r.ResponseURL += "/requests/" + r.RequestID
			r.StatusURL = r.ResponseURL + "/status"
		}},
		{"wrong result route", func(r *Reference) { r.ResponseURL += "/cancel" }},
		{"untrusted status host", func(r *Reference) { r.StatusURL = strings.Replace(r.StatusURL, queueHost, "sub.queue.fal.run", 1) }},
		{"untrusted result host", func(r *Reference) { r.ResponseURL = strings.Replace(r.ResponseURL, queueHost, "example.com", 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := validReference()
			tc.mutate(&ref)
			client := noRequests(t)
			if _, err := client.Status(context.Background(), "test-key", ref); err == nil {
				t.Fatal("Status accepted invalid reference")
			}
			if _, err := client.Result(context.Background(), "test-key", ref); err == nil {
				t.Fatal("Result accepted invalid reference")
			}
		})
	}
}

func TestSubmitRejectsUnrelatedReference(t *testing.T) {
	for _, path := range []string{
		"xai/other-app",
		"xai/grok-imagine-video/v1.5",
		"xai/grok-imagine-video/v1.5/image-to-video",
		"xai/grok-imagine-video/v1.5/text-to-video-extra",
		"xai/grok-imagine-video/v1.5/text-to-video/extra",
	} {
		t.Run(path, func(t *testing.T) {
			url := "https://queue.fal.run/" + path + "/requests/" + requestID
			client := NewClient(func(*http.Request) (*http.Response, error) {
				body, err := json.Marshal(Reference{RequestID: requestID, StatusURL: url + "/status", ResponseURL: url})
				if err != nil {
					t.Fatal(err)
				}
				return response(http.StatusOK, "application/json", string(body)), nil
			})
			if _, err := client.Submit(context.Background(), "test-key", "xai/grok-imagine-video/v1.5/text-to-video", Input{Prompt: "waves"}); err == nil {
				t.Fatal("accepted reference to an unrelated endpoint")
			}
		})
	}
}

func TestStatusRejectsInvalidPayload(t *testing.T) {
	for _, body := range []string{
		`{"status":"UNEXPECTED","request_id":"` + requestID + `"}`,
		`{"status":"IN_QUEUE","request_id":"other"}`,
		`{"status":"COMPLETED"}`,
	} {
		client := NewClient(func(*http.Request) (*http.Response, error) {
			return response(http.StatusOK, "application/json", body), nil
		})
		if _, err := client.Status(context.Background(), "test-key", validReference()); err == nil {
			t.Fatalf("accepted invalid status: %s", body)
		}
	}
}

func TestResultResponseValidation(t *testing.T) {
	const video = `{"video":{"url":"https://example.com/clip.mp4"}}`
	for _, tc := range []struct {
		name, contentType, body string
		wantErr                 bool
	}{
		{"optional metadata", "application/json", video, false},
		{"body at limit", "application/json", video + strings.Repeat(" ", maxResponseBytes-len(video)), false},
		{"body above limit", "application/json", video + strings.Repeat(" ", maxResponseBytes+1-len(video)), true},
		{"not JSON content type", "text/plain", video, true},
		{"missing content type", "", video, true},
		{"trailing JSON", "application/json", video + `{}`, true},
		{"missing video", "application/json", `{"video":null}`, true},
		{"unsafe video URL", "application/json", `{"video":{"url":"file:///clip.mp4"}}`, true},
		{"negative metadata", "application/json", `{"video":{"url":"https://example.com/clip.mp4","duration":-1}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(tc.body)}
			client := NewClient(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {tc.contentType}}, Body: body}, nil
			})
			_, err := client.Result(context.Background(), "test-key", validReference())
			if (err != nil) != tc.wantErr || !body.closed || body.read > maxResponseBytes+1 {
				t.Fatalf("Result error = %v, closed = %v, bytes read = %d", err, body.closed, body.read)
			}
		})
	}
}

func TestHTTPErrorAndRedirect(t *testing.T) {
	for _, tc := range []struct {
		status                           int
		contentType, body, code, message string
	}{
		{429, "application/problem+json", `{"detail":"try again","type":"rate_limit"}`, "rate_limit", "try again"},
		{422, "application/json", `{"detail":[{"msg":"invalid input"}]}`, "", ""},
		{500, "application/json", `{"error":"runner failed","error_type":"runner_server_error"}`, "runner_server_error", "runner failed"},
		{502, "text/html", "not JSON", "", ""},
		{307, "application/json", `{"detail":"redirect"}`, "", ""},
	} {
		calls := 0
		client := NewClient(func(*http.Request) (*http.Response, error) {
			calls++
			resp := response(tc.status, tc.contentType, tc.body)
			resp.Header.Set("Location", "https://example.com/redirect")
			return resp, nil
		})
		_, err := client.Result(context.Background(), "test-key", validReference())
		var upstream *Error
		if !errors.As(err, &upstream) || upstream.StatusCode != tc.status || upstream.Code != tc.code || upstream.Message != tc.message || calls != 1 {
			t.Fatalf("status %d: unexpected error %#v (%d requests)", tc.status, err, calls)
		}
	}
}

func TestTransportErrorIsWrapped(t *testing.T) {
	client := NewClient(func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})
	if _, err := client.Result(context.Background(), "test-key", validReference()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Result error = %v, want context.Canceled", err)
	}
}

func noRequests(t *testing.T) *Client {
	t.Helper()
	return NewClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("sent HTTP request for invalid input")
		return nil, nil
	})
}

type trackedBody struct {
	*strings.Reader
	read   int
	closed bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}
