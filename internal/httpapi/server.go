package httpapi

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/urie96/apple-music-api/internal/apple"
	"github.com/urie96/apple-music-api/internal/config"
	"github.com/urie96/apple-music-api/internal/model"
	"github.com/urie96/apple-music-api/internal/playback"
	"github.com/urie96/apple-music-api/internal/store"
)

type Server struct {
	Config   *config.Config
	Apple    *apple.Service
	Client   *apple.Client
	Playback *playback.Service
}

func New(cfg *config.Config, client *apple.Client, appleSvc *apple.Service, playbackSvc *playback.Service) *Server {
	return &Server{Config: cfg, Client: client, Apple: appleSvc, Playback: playbackSvc}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/storefront", s.handleStorefront)
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/library/playlists", s.handleLibraryPlaylists)
	mux.HandleFunc("/library/playlists/", s.handleLibraryPlaylistSubroutes)
	mux.HandleFunc("/library/albums", s.handleLibraryAlbums)
	mux.HandleFunc("/library/albums/", s.handleLibraryAlbumSubroutes)
	mux.HandleFunc("/library/artists", s.handleLibraryArtists)
	mux.HandleFunc("/library/tracks", s.handleLibraryTracks)
	mux.HandleFunc("/library/favorites/tracks", s.handleFavoriteTracks)
	mux.HandleFunc("/catalog/artists/", s.handleCatalogArtistSubroutes)
	mux.HandleFunc("/ratings/tracks/", s.handleTrackRating)
	mux.HandleFunc("/recommendations", s.handleRecommendations)
	mux.HandleFunc("/stations/", s.handleStationSubroutes)
	mux.HandleFunc("/tracks/", s.handleTrackSubroutes)
	mux.HandleFunc("/auth/apple", s.handleAuthApple)
	mux.HandleFunc("/auth/apple/callback", s.handleAuthAppleCallback)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, model.Envelope{Data: map[string]any{"status": "ok"}, Meta: map[string]any{"time": time.Now().Format(time.RFC3339)}})
}

func (s *Server) handleStorefront(w http.ResponseWriter, r *http.Request) {
	id, err := s.Apple.Storefront()
	if err != nil {
		writeError(w, http.StatusBadGateway, "apple_error", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, model.Envelope{Data: map[string]any{"id": id}})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	term := q.Get("term")
	if strings.TrimSpace(term) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing term", nil)
		return
	}
	limit, offset := limitOffset(r, 25)
	result, err := s.Apple.Search(term, q.Get("types"), limit, offset)
	if err != nil {
		writeError(w, http.StatusBadGateway, "apple_error", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, model.Envelope{Data: result, Meta: model.PaginationMeta{Limit: limit, Offset: offset}})
}

func (s *Server) handleLibraryPlaylists(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/library/playlists" {
		http.NotFound(w, r)
		return
	}
	limit, offset := limitOffset(r, 50)
	items, next, err := s.Apple.LibraryPlaylists(limit, offset)
	writeCollection(w, items, limit, offset, next, err)
}

func (s *Server) handleLibraryPlaylistSubroutes(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/library/playlists/"))
	if len(parts) == 2 && parts[1] == "tracks" {
		if r.Method == http.MethodPost {
			s.handleAddTrackToPlaylist(w, r, parts[0])
			return
		}
		limit, offset := limitOffset(r, 100)
		items, next, err := s.Apple.LibraryPlaylistTracks(parts[0], limit, offset)
		writeCollection(w, items, limit, offset, next, err)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleLibraryAlbums(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/library/albums" {
		http.NotFound(w, r)
		return
	}
	limit, offset := limitOffset(r, 50)
	items, next, err := s.Apple.LibraryAlbums(limit, offset)
	writeCollection(w, items, limit, offset, next, err)
}

func (s *Server) handleLibraryAlbumSubroutes(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/library/albums/"))
	if len(parts) == 2 && parts[1] == "tracks" {
		limit, offset := limitOffset(r, 100)
		items, next, err := s.Apple.LibraryAlbumTracks(parts[0], limit, offset)
		writeCollection(w, items, limit, offset, next, err)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleLibraryArtists(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/library/artists" {
		http.NotFound(w, r)
		return
	}
	limit, offset := limitOffset(r, 50)
	items, next, err := s.Apple.LibraryArtists(limit, offset)
	writeCollection(w, items, limit, offset, next, err)
}

func (s *Server) handleLibraryTracks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/library/tracks" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.handleAddToLibrary(w, r)
		return
	}
	limit, offset := limitOffset(r, 50)
	items, next, err := s.Apple.LibraryTracks(limit, offset)
	writeCollection(w, items, limit, offset, next, err)
}

func (s *Server) handleAddToLibrary(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TrackID string `json:"track_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TrackID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing track_id", nil)
		return
	}
	if err := s.Apple.AddToLibrary(body.TrackID); err != nil {
		writeError(w, http.StatusBadGateway, "apple_error", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, model.Envelope{Data: map[string]any{"added": body.TrackID}})
}

func (s *Server) handleFavoriteTracks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/library/favorites/tracks" {
		http.NotFound(w, r)
		return
	}
	limit, offset := limitOffset(r, 50)
	items, next, err := s.Apple.FavoriteTracks(limit, offset)
	writeCollection(w, items, limit, offset, next, err)
}

func (s *Server) handleCatalogArtistSubroutes(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/catalog/artists/"))
	if len(parts) == 2 && parts[1] == "albums" {
		limit, offset := limitOffset(r, 50)
		items, next, err := s.Apple.CatalogArtistAlbums(parts[0], limit, offset)
		writeCollection(w, items, limit, offset, next, err)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleTrackRating(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use PUT", nil)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/ratings/tracks/")
	var body struct {
		Liked bool `json:"liked"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.Apple.SetTrackRating(id, body.Liked); err != nil {
		writeError(w, http.StatusBadGateway, "apple_error", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, model.Envelope{Data: map[string]any{"id": id, "liked": body.Liked}})
}

func (s *Server) handleAddTrackToPlaylist(w http.ResponseWriter, r *http.Request, playlistID string) {
	var body struct {
		TrackID string `json:"track_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TrackID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing track_id", nil)
		return
	}
	if err := s.Apple.AddTrackToPlaylist(playlistID, body.TrackID); err != nil {
		writeError(w, http.StatusBadGateway, "apple_error", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, model.Envelope{Data: map[string]any{"playlist_id": playlistID, "track_id": body.TrackID}})
}

func (s *Server) handleRecommendations(w http.ResponseWriter, r *http.Request) {
	limit, offset := limitOffset(r, 50)
	items, next, err := s.Apple.Recommendations(limit, offset)
	writeCollection(w, items, limit, offset, next, err)
}

func (s *Server) handleStationSubroutes(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/stations/"))
	if len(parts) == 2 && parts[1] == "tracks" {
		limit, offset := limitOffset(r, 50)
		items, next, err := s.Apple.StationTracks(parts[0], limit, offset)
		writeCollection(w, items, limit, offset, next, err)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleTrackSubroutes(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/tracks/"))
	if len(parts) == 2 && parts[1] == "play" {
		s.Playback.ServeTrack(w, r, parts[0])
		return
	}
	http.NotFound(w, r)
}

func writeCollection[T any](w http.ResponseWriter, items []T, limit, offset int, hasNext bool, err error) {
	if err != nil {
		writeError(w, http.StatusBadGateway, "apple_error", err.Error(), nil)
		return
	}
	var next *int
	if hasNext {
		n := offset + limit
		next = &n
	}
	writeJSON(w, http.StatusOK, model.Envelope{Data: items, Meta: model.PaginationMeta{Limit: limit, Offset: offset, Count: len(items), Next: next}})
}

func writeJSON(w http.ResponseWriter, status int, payload model.Envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string, detail any) {
	writeJSON(w, status, model.Envelope{Error: &model.APIError{Code: code, Message: message, Detail: detail}})
}

func limitOffset(r *http.Request, defaultLimit int) (int, int) {
	limit := defaultLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}
	offset := 0
	if s := r.URL.Query().Get("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

func splitPath(path string) []string {
	var out []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

//go:embed templates/auth_apple.html templates/index.html
var templateFS embed.FS

var authPage = template.Must(template.ParseFS(templateFS, "templates/auth_apple.html"))
var indexPage = template.Must(template.ParseFS(templateFS, "templates/index.html"))

func (s *Server) handleAuthApple(w http.ResponseWriter, r *http.Request) {
	warning := ""
	if s.Config.EnvUserTokenOverridesStore() {
		warning = "A user token is currently supplied by environment variable or flag. Saving new credentials will not affect this running service until the env/flag override is removed and the service is restarted."
	}
	_ = authPage.Execute(w, map[string]any{"Warning": warning, "AppToken": maskIfEnv(s.Config.Credential.AppToken, s.Config.Credential.AppTokenSource)})
}

func (s *Server) handleAuthAppleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", nil)
		return
	}
	var body struct {
		AppToken           string `json:"app_token"`
		UserToken          string `json:"user_token"`
		UserTokenUpdatedAt int64  `json:"user_token_updated_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AppToken == "" || body.UserToken == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "app_token and user_token are required", nil)
		return
	}
	cred := store.AppleCredential{AppToken: body.AppToken, UserToken: body.UserToken, UserTokenUpdatedAt: body.UserTokenUpdatedAt}
	if err := s.Config.Store.Save(cred); err != nil {
		writeError(w, http.StatusInternalServerError, "credential_store_error", err.Error(), nil)
		return
	}
	applied := false
	if s.Config.Credential.AppTokenSource != config.SourceEnv && s.Config.Credential.AppTokenSource != config.SourceFlag &&
		s.Config.Credential.UserTokenSource != config.SourceEnv && s.Config.Credential.UserTokenSource != config.SourceFlag {
		s.Client.AppToken = body.AppToken
		s.Client.UserToken = body.UserToken
		s.Config.Credential.AppToken = body.AppToken
		s.Config.Credential.UserToken = body.UserToken
		s.Config.Credential.AppTokenSource = config.SourceStore
		s.Config.Credential.UserTokenSource = config.SourceStore
		applied = true
	}
	writeJSON(w, http.StatusOK, model.Envelope{Data: map[string]any{"saved": true, "applied_to_runtime": applied, "credential_store": s.Config.CredentialPath}})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := map[string]any{
		"Addr":            s.Config.Addr(),
		"Authorized":      s.Config.HasCredential(),
		"CredentialPath":  s.Config.CredentialPath,
		"AppTokenSource":  string(s.Config.Credential.AppTokenSource),
		"UserTokenSource": string(s.Config.Credential.UserTokenSource),
	}

	if s.Config.Credential.AppToken != "" {
		data["AppTokenPreview"] = maskPreview(s.Config.Credential.AppToken, 20)
	}
	if s.Config.Credential.UserToken != "" {
		data["UserTokenPreview"] = maskPreview(s.Config.Credential.UserToken, 20)
	}
	if s.Config.Credential.UserTokenUpdatedAt > 0 {
		data["UserTokenUpdatedAt"] = time.Unix(s.Config.Credential.UserTokenUpdatedAt, 0).Format("2006-01-02 15:04:05")
	}
	if s.Config.EnvUserTokenOverridesStore() {
		data["Warning"] = "A user token is currently supplied by environment variable or flag. The credential store is not in effect."
	}

	// Only try storefront if authorized
	if s.Config.HasCredential() {
		id, err := s.Apple.Storefront()
		if err != nil {
			data["StorefrontError"] = err.Error()
		} else {
			data["Storefront"] = id
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = indexPage.Execute(w, data)
}

func maskPreview(value string, visible int) string {
	if len(value) <= visible {
		return value
	}
	return value[:visible] + "…"
}

func maskIfEnv(value string, source config.Source) string {
	if source == config.SourceStore || source == config.SourceEmpty {
		return value
	}
	return ""
}

func debugf(format string, args ...any) { fmt.Printf(format, args...) }
