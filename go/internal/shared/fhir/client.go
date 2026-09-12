package fhir

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Result is the outcome of one GET against a FHIR server.
type Result struct {
	// URL is the requested URL before any rewrite.
	URL string
	// Status is the HTTP status, or 0 when no response was received.
	Status int
	// Resource is the decoded JSON object of a 2xx response.
	Resource map[string]any
	// Duration is the elapsed time of the call.
	Duration time.Duration
	// Err is a transport error, timeout, or an unparseable 2xx body.
	Err error
}

// Endpoint is a FHIR base URL and an optional bearer token.
type Endpoint struct {
	BaseURL     string
	AccessToken string
	TokenType   string
}

// Client performs FHIR GET requests. The zero value uses http.DefaultClient.
type Client struct {
	HTTP *http.Client
	// Rewrite maps outbound URLs, e.g. localhost to host.docker.internal.
	Rewrite func(string) string
	// Timeout bounds each request when positive.
	Timeout time.Duration
}

// Get requests {base}/{path}?{query} with Accept: application/fhir+json.
func (c *Client) Get(ctx context.Context, ep Endpoint, path string, query url.Values) (res Result) {
	res.URL = strings.TrimRight(ep.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		res.URL += "?" + query.Encode()
	}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	target := res.URL
	if c.Rewrite != nil {
		target = c.Rewrite(target)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res.Err = err
		return res
	}
	req.Header.Set("Accept", "application/fhir+json")
	if ep.AccessToken != "" {
		tt := ep.TokenType
		if tt == "" || strings.EqualFold(tt, "bearer") {
			tt = "Bearer"
		}
		req.Header.Set("Authorization", tt+" "+ep.AccessToken)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		res.Err = rootCause(err)
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		res.Err = rootCause(err)
		return res
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return res
	}
	v, err := DecodeOrdered(body)
	obj, ok := v.(map[string]any)
	if err != nil || !ok {
		res.Err = errors.New("response body is not FHIR JSON")
		return res
	}
	res.Resource = obj
	return res
}

func rootCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}
