package farm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

type Client struct {
	URL, Token string
	HTTP       *http.Client
}

func NewClient(url, token string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &Client{strings.TrimRight(url, "/"), token, &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) Request(ctx context.Context, method, path, attempt string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if attempt != "" {
		req.Header.Set("X-Attempt-Token", attempt)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		return nil, &HTTPError{Status: res.StatusCode, Message: fmt.Sprintf("%s %s: HTTP %d: %s", method, path, res.StatusCode, strings.TrimSpace(string(msg)))}
	}
	return res, nil
}
func (c *Client) JSON(ctx context.Context, method, path, attempt string, input, output any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	res, err := c.Request(ctx, method, path, attempt, body)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if output != nil && res.StatusCode != http.StatusNoContent {
		err = json.NewDecoder(res.Body).Decode(output)
	}
	return res.StatusCode, err
}
