package apple

import "encoding/json"

type Resource struct {
	ID            string                  `json:"id"`
	Type          string                  `json:"type"`
	Href          string                  `json:"href,omitempty"`
	Attributes    map[string]any          `json:"attributes,omitempty"`
	Relationships map[string]Relationship `json:"relationships,omitempty"`
	Views         map[string]Relationship `json:"views,omitempty"`
	Raw           json.RawMessage         `json:"-"`
}

type Relationship struct {
	Data []Resource `json:"data,omitempty"`
	Href string     `json:"href,omitempty"`
	Next string     `json:"next,omitempty"`
}

type ListResponse struct {
	Data []Resource     `json:"data,omitempty"`
	Next string         `json:"next,omitempty"`
	Raw  map[string]any `json:"-"`
}

type SearchBlock struct {
	Data []Resource `json:"data,omitempty"`
	Next string     `json:"next,omitempty"`
}

type SearchResponse struct {
	Results map[string]SearchBlock `json:"results,omitempty"`
	Raw     map[string]any         `json:"-"`
}

type WebPlaybackSong struct {
	Assets          []WebPlaybackAsset `json:"assets"`
	HLSKeyServerURL string             `json:"hls-key-server-url"`
}

type WebPlaybackAsset struct {
	URL    string `json:"URL"`
	Flavor string `json:"flavor"`
}

type WebPlaybackResponse struct {
	SongList []WebPlaybackSong `json:"songList"`
}
