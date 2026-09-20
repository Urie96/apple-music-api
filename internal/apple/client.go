package apple

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/urie96/apple-music-api/internal/config"
)

const (
	APIBase        = "https://api.music.apple.com/v1"
	WebPlaybackURL = "https://play.music.apple.com/WebObjects/MZPlay.woa/wa/webPlayback"
)

type Client struct {
	AppToken  string
	UserToken string
	HTTP      *http.Client
}

func NewClient(cred config.Credential) *Client {
	return &Client{
		AppToken:  cred.AppToken,
		UserToken: cred.UserToken,
		HTTP:      &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *Client) HasCredential() bool { return c.AppToken != "" && c.UserToken != "" }

func (c *Client) headers(contentType string) http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+c.AppToken)
	h.Set("Music-User-Token", c.UserToken)
	h.Set("Accept", "application/json")
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return h
}

func (c *Client) playbackHeaders() http.Header {
	h := c.headers("application/json;charset=utf-8")
	h.Set("Media-User-Token", c.UserToken)
	h.Set("Origin", "https://music.apple.com")
	h.Set("Referer", "https://music.apple.com/")
	h.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	return h
}

func (c *Client) Get(path string, params url.Values, out any) error {
	return c.request("GET", path, params, nil, out)
}

func (c *Client) Post(path string, params url.Values, body any, out any) error {
	return c.request("POST", path, params, body, out)
}

func (c *Client) Put(path string, params url.Values, body any, out any) error {
	return c.request("PUT", path, params, body, out)
}

func (c *Client) request(method, path string, params url.Values, body any, out any) error {
	if !c.HasCredential() {
		return fmt.Errorf("missing Apple credential")
	}
	u := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		u = APIBase + "/" + strings.TrimLeft(path, "/")
	}
	if len(params) > 0 {
		if strings.Contains(u, "?") {
			u += "&" + params.Encode()
		} else {
			u += "?" + params.Encode()
		}
	}
	var rbody io.Reader
	contentType := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rbody = bytes.NewReader(data)
		contentType = "application/json"
	}
	req, err := http.NewRequest(method, u, rbody)
	if err != nil {
		return err
	}
	req.Header = c.headers(contentType)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Apple Music HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode Apple response: %w", err)
	}
	return nil
}

func (c *Client) List(path string, params url.Values) (ListResponse, error) {
	var out ListResponse
	err := c.Get(path, params, &out)
	return out, err
}

func (c *Client) Search(storefront, term, types string, limit, offset int) (SearchResponse, error) {
	params := url.Values{}
	params.Set("term", strings.ReplaceAll(term, "'", ""))
	params.Set("types", types)
	if limit > 0 {
		params.Set("limit", fmt.Sprint(limit))
	}
	if offset > 0 {
		params.Set("offset", fmt.Sprint(offset))
	}
	var out SearchResponse
	err := c.Get("catalog/"+storefront+"/search", params, &out)
	return out, err
}

func (c *Client) Storefront() (string, error) {
	out, err := c.List("me/storefront", nil)
	if err != nil {
		return "", err
	}
	if len(out.Data) == 0 || out.Data[0].ID == "" {
		return "", fmt.Errorf("empty storefront response")
	}
	return out.Data[0].ID, nil
}

func (c *Client) WebPlayback(trackID string, isLibrary bool) (*WebPlaybackSong, error) {
	body := map[string]any{}
	if isLibrary {
		body["universalLibraryId"] = trackID
		body["isLibrary"] = true
	} else {
		body["salableAdamId"] = trackID
	}
	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", WebPlaybackURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header = c.playbackHeaders()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webPlayback request: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("webPlayback HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var result WebPlaybackResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode webPlayback: %w", err)
	}
	if len(result.SongList) == 0 {
		return nil, fmt.Errorf("empty songList")
	}
	return &result.SongList[0], nil
}

func (c *Client) RequestAppleLicense(licenseURL, skdURI string, challenge []byte, trackID string) ([]byte, error) {
	body := map[string]any{
		"challenge":      base64.StdEncoding.EncodeToString(challenge),
		"key-system":     "com.widevine.alpha",
		"uri":            skdURI,
		"adamId":         trackID,
		"isLibrary":      false,
		"user-initiated": true,
	}
	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", licenseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header = c.playbackHeaders()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST license: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("license server HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var result struct {
		License string `json:"license"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode license response: %w", err)
	}
	if result.License == "" {
		return nil, fmt.Errorf("license field empty in response")
	}
	return base64.StdEncoding.DecodeString(result.License)
}
