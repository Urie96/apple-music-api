package model

// Envelope is the public JSON response wrapper used by every JSON API route.
type Envelope struct {
	Data  any       `json:"data,omitempty"`
	Meta  any       `json:"meta,omitempty"`
	Error *APIError `json:"error,omitempty"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

type PaginationMeta struct {
	Limit  int  `json:"limit"`
	Offset int  `json:"offset"`
	Count  int  `json:"count"`
	Next   *int `json:"next,omitempty"`
}

type EntityKind string

const (
	KindCatalog EntityKind = "catalog"
	KindLibrary EntityKind = "library"
	KindStation EntityKind = "station"
)

type Track struct {
	Type       string     `json:"type"`
	ID         string     `json:"id"`
	Kind       EntityKind `json:"kind"`
	Title      string     `json:"title"`
	Artist     string     `json:"artist,omitempty"`
	Album      string     `json:"album,omitempty"`
	Duration   int        `json:"duration,omitempty"`
	Liked      bool       `json:"liked"`
	TrackNo    int        `json:"track_no,omitempty"`
	DiscNo     int        `json:"disc_no,omitempty"`
	Genre      string     `json:"genre,omitempty"`
	ArtworkURL string     `json:"artwork_url,omitempty"`
	PlayURL    string     `json:"play_url,omitempty"`
	Raw        any        `json:"raw,omitempty"`
}

type Playlist struct {
	Type        string     `json:"type"`
	ID          string     `json:"id"`
	Kind        EntityKind `json:"kind"`
	Name        string     `json:"name"`
	Owner       string     `json:"owner,omitempty"`
	TrackCount  int        `json:"track_count,omitempty"`
	Description string     `json:"description,omitempty"`
	ArtworkURL  string     `json:"artwork_url,omitempty"`
	CanEdit     bool       `json:"can_edit,omitempty"`
	Dynamic     bool       `json:"dynamic,omitempty"`
	SourceTitle string     `json:"source_title,omitempty"`
	Raw         any        `json:"raw,omitempty"`
}

type Album struct {
	Type        string     `json:"type"`
	ID          string     `json:"id"`
	Kind        EntityKind `json:"kind"`
	Name        string     `json:"name"`
	Artist      string     `json:"artist,omitempty"`
	Year        int        `json:"year,omitempty"`
	TrackCount  int        `json:"track_count,omitempty"`
	Genre       string     `json:"genre,omitempty"`
	Description string     `json:"description,omitempty"`
	ArtworkURL  string     `json:"artwork_url,omitempty"`
	Raw         any        `json:"raw,omitempty"`
}

type Artist struct {
	Type        string     `json:"type"`
	ID          string     `json:"id"`
	Kind        EntityKind `json:"kind"`
	Name        string     `json:"name"`
	Genre       string     `json:"genre,omitempty"`
	Description string     `json:"description,omitempty"`
	ArtworkURL  string     `json:"artwork_url,omitempty"`
	Raw         any        `json:"raw,omitempty"`
}

type SearchResults struct {
	Tracks    []Track    `json:"tracks"`
	Albums    []Album    `json:"albums"`
	Artists   []Artist   `json:"artists"`
	Playlists []Playlist `json:"playlists"`
}
