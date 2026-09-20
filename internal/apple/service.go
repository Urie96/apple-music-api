package apple

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/urie96/apple-music-api/internal/model"
)

type Service struct {
	Client     *Client
	storefront string
}

func NewService(client *Client) *Service { return &Service{Client: client} }

func (s *Service) Storefront() (string, error) {
	if s.storefront != "" {
		return s.storefront, nil
	}
	id, err := s.Client.Storefront()
	if err != nil {
		return "", err
	}
	s.storefront = id
	return id, nil
}

func pageParams(limit, offset int) url.Values {
	v := url.Values{}
	if limit > 0 {
		v.Set("limit", fmt.Sprint(limit))
	}
	if offset > 0 {
		v.Set("offset", fmt.Sprint(offset))
	}
	return v
}

func (s *Service) LibraryPlaylists(limit, offset int) ([]model.Playlist, bool, error) {
	resp, err := s.Client.List("me/library/playlists", pageParams(limit, offset))
	if err != nil {
		return nil, false, err
	}
	out := make([]model.Playlist, 0, len(resp.Data))
	for _, r := range resp.Data {
		out = append(out, NormalizePlaylist(r))
	}
	return out, resp.Next != "", nil
}

func (s *Service) LibraryPlaylistTracks(id string, limit, offset int) ([]model.Track, bool, error) {
	path := "me/library/playlists/" + url.PathEscape(id) + "/tracks"
	params := withInclude(pageParams(limit, offset), "artists,catalog")
	if !IsLibraryID(id) && !strings.HasPrefix(id, "ra.") {
		storefront, err := s.Storefront()
		if err != nil {
			return nil, false, err
		}
		path = "catalog/" + storefront + "/playlists/" + url.PathEscape(id) + "/tracks"
		params = withInclude(pageParams(limit, offset), "artists")
	}
	resp, err := s.Client.List(path, params)
	if err != nil {
		return nil, false, err
	}
	out := make([]model.Track, 0, len(resp.Data))
	for _, r := range resp.Data {
		out = append(out, NormalizeTrack(r))
	}
	_ = s.applyTrackRatings(out)
	return out, resp.Next != "", nil
}

func (s *Service) LibraryAlbums(limit, offset int) ([]model.Album, bool, error) {
	resp, err := s.Client.List("me/library/albums", withInclude(pageParams(limit, offset), "catalog,artists"))
	if err != nil {
		return nil, false, err
	}
	out := make([]model.Album, 0, len(resp.Data))
	for _, r := range resp.Data {
		out = append(out, NormalizeAlbum(r))
	}
	return out, resp.Next != "", nil
}

func (s *Service) LibraryArtists(limit, offset int) ([]model.Artist, bool, error) {
	resp, err := s.Client.List("me/library/artists", withInclude(pageParams(limit, offset), "catalog"))
	if err != nil {
		return nil, false, err
	}
	out := make([]model.Artist, 0, len(resp.Data))
	for _, r := range resp.Data {
		out = append(out, NormalizeArtist(r))
	}
	return out, resp.Next != "", nil
}

func (s *Service) LibraryTracks(limit, offset int) ([]model.Track, bool, error) {
	resp, err := s.Client.List("me/library/songs", withInclude(pageParams(limit, offset), "catalog,albums,artists"))
	if err != nil {
		return nil, false, err
	}
	out := make([]model.Track, 0, len(resp.Data))
	for _, r := range resp.Data {
		out = append(out, NormalizeTrack(r))
	}
	_ = s.applyTrackRatings(out)
	return out, resp.Next != "", nil
}

func (s *Service) FavoriteTracks(limit, offset int) ([]model.Track, bool, error) {
	tracks, next, err := s.LibraryTracks(limit, offset)
	if err != nil {
		return nil, false, err
	}
	out := tracks[:0]
	for _, track := range tracks {
		if track.Liked {
			out = append(out, track)
		}
	}
	return out, next, nil
}

func (s *Service) LibraryAlbumTracks(id string, limit, offset int) ([]model.Track, bool, error) {
	path := "me/library/albums/" + url.PathEscape(id) + "/tracks"
	params := withInclude(pageParams(limit, offset), "artists,catalog")
	if !IsLibraryID(id) {
		storefront, err := s.Storefront()
		if err != nil {
			return nil, false, err
		}
		path = "catalog/" + storefront + "/albums/" + url.PathEscape(id) + "/tracks"
		params = withInclude(pageParams(limit, offset), "artists")
	}
	resp, err := s.Client.List(path, params)
	if err != nil {
		return nil, false, err
	}
	out := make([]model.Track, 0, len(resp.Data))
	for _, r := range resp.Data {
		out = append(out, NormalizeTrack(r))
	}
	_ = s.applyTrackRatings(out)
	return out, resp.Next != "", nil
}

func (s *Service) CatalogArtistAlbums(id string, limit, offset int) ([]model.Album, bool, error) {
	resolvedID := id
	if IsLibraryID(id) {
		resp, err := s.Client.List("me/library/artists/"+url.PathEscape(id), withInclude(url.Values{}, "catalog"))
		if err != nil {
			return nil, false, fmt.Errorf("resolve library artist: %w", err)
		}
		if len(resp.Data) > 0 {
			if cat := FirstRelationshipResource(resp.Data[0], "catalog"); cat != nil {
				resolvedID = cat.ID
			}
		}
	}
	storefront, err := s.Storefront()
	if err != nil {
		return nil, false, err
	}
	resp, err := s.Client.List("catalog/"+storefront+"/artists/"+url.PathEscape(resolvedID)+"/albums", pageParams(limit, offset))
	if err != nil {
		// Apple returns 404 when the artist has no albums in this storefront.
		// This is a normal state, not a service error.
		if strings.Contains(err.Error(), "No related resources") || strings.Contains(err.Error(), "40403") {
			return nil, false, nil
		}
		return nil, false, err
	}
	out := make([]model.Album, 0, len(resp.Data))
	for _, r := range resp.Data {
		out = append(out, NormalizeAlbum(r))
	}
	return out, resp.Next != "", nil
}

func (s *Service) Search(term, types string, limit, offset int) (model.SearchResults, error) {
	storefront, err := s.Storefront()
	if err != nil {
		return model.SearchResults{}, err
	}
	if strings.TrimSpace(types) == "" {
		types = "songs,albums,artists,playlists"
	}
	resp, err := s.Client.Search(storefront, term, types, limit, offset)
	if err != nil {
		return model.SearchResults{}, err
	}
	var out model.SearchResults
	for _, r := range resp.Results["songs"].Data {
		out.Tracks = append(out.Tracks, NormalizeTrack(r))
	}
	_ = s.applyTrackRatings(out.Tracks)
	for _, r := range resp.Results["albums"].Data {
		out.Albums = append(out.Albums, NormalizeAlbum(r))
	}
	for _, r := range resp.Results["artists"].Data {
		out.Artists = append(out.Artists, NormalizeArtist(r))
	}
	for _, r := range resp.Results["playlists"].Data {
		out.Playlists = append(out.Playlists, NormalizePlaylist(r))
	}
	return out, nil
}

func (s *Service) Recommendations(limit, offset int) ([]model.Playlist, bool, error) {
	params := url.Values{}
	params.Set("include[personal-recommendation]", "contents")
	resp, err := s.Client.List("me/recommendations", params)
	if err != nil {
		return nil, false, err
	}
	var out []model.Playlist
	for _, rec := range resp.Data {
		title := "Recommendations"
		if titleMap, ok := rec.Attributes["title"].(map[string]any); ok {
			if s, ok := titleMap["stringForDisplay"].(string); ok && s != "" {
				title = s
			}
		}
		if contents, ok := rec.Relationships["contents"]; ok {
			for _, item := range contents.Data {
				switch item.Type {
				case "stations":
					out = append(out, NormalizeStation(item, title))
				case "playlists", "library-playlists":
					pl := NormalizePlaylist(item)
					pl.SourceTitle = title
					out = append(out, pl)
				}
			}
		}
	}
	// Slice in-memory: me/recommendations does not support limit/offset.
	if offset > 0 && offset < len(out) {
		out = out[offset:]
	} else if offset >= len(out) {
		out = nil
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, resp.Next != "", nil
}

func (s *Service) StationTracks(id string, limit, offset int) ([]model.Track, bool, error) {
	params := url.Values{}
	params.Set("include", "artists")
	var resp ListResponse
	if err := s.Client.Post("me/stations/next-tracks/"+url.PathEscape(id), params, map[string]any{}, &resp); err != nil {
		return nil, false, err
	}
	data := resp.Data
	if offset > 0 && offset < len(data) {
		data = data[offset:]
	} else if offset >= len(data) {
		data = nil
	}
	if limit > 0 && len(data) > limit {
		data = data[:limit]
	}
	out := make([]model.Track, 0, len(data))
	for _, r := range data {
		out = append(out, NormalizeTrack(r))
	}
	_ = s.applyTrackRatings(out)
	return out, false, nil
}

func (s *Service) SetTrackRating(id string, liked bool) error {
	typ := "songs"
	if IsLibraryID(id) {
		typ = "library-songs"
	}
	body := map[string]any{"type": "ratings", "attributes": map[string]any{"value": -1}}
	if liked {
		body["attributes"] = map[string]any{"value": 1}
	}
	return s.Client.Put("me/ratings/"+typ+"/"+url.PathEscape(id), nil, body, nil)
}

func (s *Service) AddTrackToPlaylist(playlistID, trackID string) error {
	typ := "songs"
	if IsLibraryID(trackID) {
		typ = "library-songs"
	}
	body := map[string]any{"data": []map[string]any{{"id": trackID, "type": typ}}}
	return s.Client.Post("me/library/playlists/"+url.PathEscape(playlistID)+"/tracks", nil, body, nil)
}

// AddToLibrary adds a catalog track (by catalog ID) to the user's library.
// Returns nil even if the track is already in the library (Apple returns 202 in that case).
func (s *Service) AddToLibrary(trackID string) error {
	params := url.Values{}
	params.Set("ids[songs]", trackID)
	return s.Client.Post("me/library", params, nil, nil)
}

func (s *Service) applyTrackRatings(tracks []model.Track) error {
	var catalogIDs, libraryIDs []string
	for _, t := range tracks {
		if t.ID == "" {
			continue
		}
		if IsLibraryID(t.ID) {
			libraryIDs = append(libraryIDs, t.ID)
		} else {
			catalogIDs = append(catalogIDs, t.ID)
		}
	}
	ratings := map[string]bool{}
	if err := s.collectRatings("songs", catalogIDs, ratings); err != nil {
		return err
	}
	if err := s.collectRatings("library-songs", libraryIDs, ratings); err != nil {
		return err
	}
	for i := range tracks {
		tracks[i].Liked = ratings[tracks[i].ID]
	}
	return nil
}

func (s *Service) collectRatings(kind string, ids []string, out map[string]bool) error {
	for len(ids) > 0 {
		batch := ids
		if len(batch) > 200 {
			batch = ids[:200]
		}
		ids = ids[len(batch):]
		params := url.Values{"ids": []string{strings.Join(batch, ",")}}
		resp, err := s.Client.List("me/ratings/"+kind, params)
		if err != nil {
			return err
		}
		for _, r := range resp.Data {
			out[r.ID] = intAttr(r.Attributes, "value") == 1
		}
	}
	return nil
}

func withInclude(v url.Values, include string) url.Values {
	if include != "" {
		v.Set("include", include)
	}
	return v
}
