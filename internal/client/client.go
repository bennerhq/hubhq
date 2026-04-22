package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	host       string
	port       int
	token      string
	httpClient *http.Client
}

type PairingResult struct {
	Token string `json:"access_token"`
}

type HubError struct {
	StatusCode int
	Status     string
	Message    string
	Body       string
}

type Home struct {
	Devices []map[string]any `json:"devices"`
	Scenes  []map[string]any `json:"scenes"`
}

type DebugFunc func(format string, args ...any)

func (e *HubError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("hub returned %s: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("hub returned %s", e.Status)
}

func New(host string, port int, token string) *Client {
	return &Client{
		host:  host,
		port:  port,
		token: token,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		},
	}
}

func (c *Client) Authenticate(ctx context.Context) (string, error) {
	return c.AuthenticateDebug(ctx, nil)
}

func (c *Client) AuthenticateDebug(ctx context.Context, debugf DebugFunc) (string, error) {
	deadline := time.Now().Add(45 * time.Second)
	verifier, challenge, err := pkce()
	if err != nil {
		return "", err
	}
	debug(debugf, "starting pairing flow against %s:%d", c.host, c.port)

	values := url.Values{}
	values.Set("audience", "homesmart.local")
	values.Set("response_type", "code")
	values.Set("code_challenge", challenge)
	values.Set("code_challenge_method", "S256")

	for {
		authResp, err := c.authorizeWithRetry(ctx, values, deadline, debugf)
		if err != nil {
			return "", err
		}
		if authResp.Code == "" {
			return "", errors.New("pairing code missing in authorize response")
		}
		debug(debugf, "authorize succeeded; received code")

		form := url.Values{}
		form.Set("code", authResp.Code)
		form.Set("name", hostname())
		form.Set("grant_type", "authorization_code")
		form.Set("code_verifier", verifier)

		var tokenResp PairingResult
		debug(debugf, "requesting access token")
		if err := c.postForm(ctx, "/v1/oauth/token", form, &tokenResp); err != nil {
			var hubErr *HubError
			if errors.As(err, &hubErr) && isRetryablePairingError(hubErr) && time.Now().Before(deadline) {
				debug(debugf, "token retry after %s: %s", hubErr.Status, hubErr.Message)
				if err := waitOrCancel(ctx, 2*time.Second); err != nil {
					return "", err
				}
				continue
			}
			return "", err
		}
		if tokenResp.Token == "" {
			return "", errors.New("access token missing in token response")
		}
		debug(debugf, "token exchange succeeded")
		return tokenResp.Token, nil
	}
}

func (c *Client) authorizeWithRetry(ctx context.Context, values url.Values, deadline time.Time, debugf DebugFunc) (*struct {
	Code string `json:"code"`
}, error) {
	for {
		var authResp struct {
			Code string `json:"code"`
		}
		debug(debugf, "requesting authorize code")
		err := c.getForm(ctx, "/v1/oauth/authorize", values, &authResp)
		if err == nil {
			return &authResp, nil
		}

		var hubErr *HubError
		if !errors.As(err, &hubErr) {
			return nil, err
		}
		if !isRetryablePairingError(hubErr) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("pairing did not become ready within 45 seconds: %w", err)
		}
		debug(debugf, "authorize retry after %s: %s", hubErr.Status, hubErr.Message)

		if err := waitOrCancel(ctx, 2*time.Second); err != nil {
			return nil, err
		}
	}
}

func isRetryablePairingError(err *HubError) bool {
	body := strings.ToLower(err.Body)
	switch err.StatusCode {
	case http.StatusConflict:
		return strings.Contains(body, "ongoing pairing request")
	case http.StatusForbidden:
		return strings.Contains(body, "button not pressed") || strings.Contains(body, "presence time stamp timed out")
	default:
		return false
	}
}

func waitOrCancel(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func debug(debugf DebugFunc, format string, args ...any) {
	if debugf == nil {
		return
	}
	allArgs := append([]any{time.Now().Format(time.RFC3339)}, args...)
	debugf("[%s] "+format, allArgs...)
}

func (c *Client) Home(ctx context.Context) (*Home, error) {
	var home Home
	if err := c.Get(ctx, "/v1/home", &home); err == nil {
		return &home, nil
	}

	var devices []map[string]any
	if err := c.Get(ctx, "/v1/devices", &devices); err != nil {
		return nil, err
	}
	var scenes []map[string]any
	if err := c.Get(ctx, "/v1/scenes", &scenes); err != nil {
		return nil, err
	}
	return &Home{Devices: devices, Scenes: scenes}, nil
}

func (c *Client) Get(ctx context.Context, path string, into any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, into)
}

func (c *Client) Patch(ctx context.Context, path string, body any, into any) error {
	return c.doJSON(ctx, http.MethodPatch, path, body, into)
}

func (c *Client) Post(ctx context.Context, path string, body any, into any) error {
	return c.doJSON(ctx, http.MethodPost, path, body, into)
}

func (c *Client) Delete(ctx context.Context, path string) error {
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) Watch(ctx context.Context, fn func(json.RawMessage) error) error {
	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	var lastErr error
	candidates := []string{
		fmt.Sprintf("wss://%s:%d/v1", c.host, c.port),
		fmt.Sprintf("wss://%s:%d/v1/events", c.host, c.port),
		fmt.Sprintf("wss://%s:%d/events", c.host, c.port),
		fmt.Sprintf("wss://%s:%d/ws", c.host, c.port),
	}
	for _, endpoint := range candidates {
		header := http.Header{}
		header.Set("Authorization", "Bearer "+c.token)
		conn, _, err := dialer.DialContext(ctx, endpoint, header)
		if err != nil {
			lastErr = err
			continue
		}
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-done:
			}
		}()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				close(done)
				if errors.Is(ctx.Err(), context.Canceled) {
					return nil
				}
				return err
			}
			if err := fn(json.RawMessage(data)); err != nil {
				close(done)
				return err
			}
		}
	}
	if lastErr != nil {
		return fmt.Errorf("connect websocket: %w", lastErr)
	}
	return errors.New("no websocket endpoint candidates configured")
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, into any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, into)
}

func (c *Client) getForm(ctx context.Context, path string, values url.Values, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path)+"?"+values.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, into)
}

func (c *Client) postForm(ctx context.Context, path string, values url.Values, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, into)
}

func (c *Client) url(path string) string {
	return fmt.Sprintf("https://%s:%d%s", c.host, c.port, path)
}

func decodeResponse(resp *http.Response, into any) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawBody := strings.TrimSpace(string(body))
		message := rawBody
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			if value, ok := payload["error"].(string); ok && value != "" {
				message = value
			}
		}
		return &HubError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Message:    message,
			Body:       rawBody,
		}
	}
	if into == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func pkce() (verifier string, challenge string, err error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	raw := make([]byte, 128)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, v := range raw {
		b.WriteByte(alphabet[int(v)%len(alphabet)])
	}
	verifier = b.String()
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "hubhq"
	}
	return name
}
