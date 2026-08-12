// Package sentryapi implements domain.SentryAPI over the Sentry REST API.
//
// It is deliberately tiny. The webhook payload already carries everything the
// nightwatcher needs except one thing: the environment, which is not in the payload
// and costs a separate tags call. Fetching the issue detail would add nothing —
// it is the same serializer as the webhook plus three URLs — so calling it would
// spend rate limit for no information.
package sentryapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// DefaultBaseURL is the Sentry SaaS API root.
const DefaultBaseURL = "https://sentry.io/api/0"

// Client talks to the Sentry REST API.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New builds a Client with a sane timeout. The timeout matters: this call sits on
// the analysis path, and a Sentry that hangs must not hold a worker slot open.
func New(token string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		Token:   token,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

var _ domain.SentryAPI = (*Client)(nil)

type tagResponse struct {
	Key       string `json:"key"`
	TopValues []struct {
		Value string `json:"value"`
		Count int64  `json:"count"`
	} `json:"topValues"`
}

// Environments returns the environment tag values seen for an issue, most frequent
// first. A 404 means the tag was never recorded for that issue, which is not an
// error — it just means no environment is known.
func (c *Client) Environments(ctx context.Context, issueID string) ([]string, error) {
	if strings.TrimSpace(issueID) == "" {
		return nil, fmt.Errorf("issueID vazio")
	}

	endpoint := fmt.Sprintf("%s/issues/%s/tags/environment/", strings.TrimRight(c.BaseURL, "/"), url.PathEscape(issueID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("montando requisição de tags: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")

	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}

	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alcançando o Sentry: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// A tag was never recorded for this issue. Not knowing the environment is a
		// normal state, not a failure.
		return nil, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("rate limit do Sentry ao ler tags da issue %s", issueID)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("Sentry devolveu %s ao ler tags da issue %s", resp.Status, issueID)
	}

	var tr tagResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("decodificando tags da issue %s: %w", issueID, err)
	}

	out := make([]string, 0, len(tr.TopValues))
	for _, v := range tr.TopValues {
		if v.Value != "" {
			out = append(out, v.Value)
		}
	}
	return out, nil
}

// Static is the test substitute: a canned answer per issue, so the analysis path can
// be exercised without a network.
type Static map[string][]string

var _ domain.SentryAPI = (Static)(nil)

func (s Static) Environments(_ context.Context, issueID string) ([]string, error) {
	return s[issueID], nil
}
