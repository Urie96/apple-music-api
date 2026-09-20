package apple

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/urie96/apple-music-api/internal/model"
)

var libraryIDRe = regexp.MustCompile(`^[ailp]\.[A-Za-z0-9]+$`)

func IsLibraryID(id string) bool { return libraryIDRe.MatchString(id) }

func KindFor(id string, typ string) model.EntityKind {
	if strings.HasPrefix(id, "ra.") || typ == "stations" {
		return model.KindStation
	}
	if IsLibraryID(id) || strings.HasPrefix(typ, "library-") {
		return model.KindLibrary
	}
	return model.KindCatalog
}

func attrs(r Resource) map[string]any {
	if cat := FirstRelationshipResource(r, "catalog"); cat != nil && len(cat.Attributes) > 0 {
		return cat.Attributes
	}
	return r.Attributes
}

// catalogDisplayID prefers the catalog ID from relationships when available.
// For library resources that have a catalog counterpart, this returns the stable
// cross-user catalog ID instead of the per-user library ID (e.g. 202968168 vs i.xxx).
func catalogDisplayID(r Resource) string {
	if cat := FirstRelationshipResource(r, "catalog"); cat != nil && cat.ID != "" {
		return cat.ID
	}
	return r.ID
}

// displayID returns the original resource ID (library ID for write operations).
func displayID(r Resource) string { return r.ID }

func FirstRelationshipResource(r Resource, name string) *Resource {
	rel, ok := r.Relationships[name]
	if !ok || len(rel.Data) == 0 {
		return nil
	}
	return &rel.Data[0]
}

func stringAttr(a map[string]any, key string) string {
	if v, ok := a[key].(string); ok {
		return v
	}
	return ""
}

func boolAttr(a map[string]any, key string) bool {
	if v, ok := a[key].(bool); ok {
		return v
	}
	return false
}

func intAttr(a map[string]any, key string) int {
	switch v := a[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	default:
		return 0
	}
}

func genre(a map[string]any) string {
	arr, ok := a["genreNames"].([]any)
	if !ok {
		return ""
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

func description(a map[string]any) string {
	for _, key := range []string{"description", "editorialNotes"} {
		m, ok := a[key].(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["standard"].(string); ok && s != "" {
			return s
		}
		if s, ok := m["short"].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func artwork(a map[string]any, size int) string {
	m, ok := a["artwork"].(map[string]any)
	if !ok {
		return ""
	}
	u, _ := m["url"].(string)
	if u == "" {
		return ""
	}
	if size <= 0 {
		size = 600
	}
	return strings.NewReplacer("{w}", fmt.Sprint(size), "{h}", fmt.Sprint(size)).Replace(u)
}

func artistNamesFromRelationship(r Resource) []string {
	rel, ok := r.Relationships["artists"]
	if !ok {
		return nil
	}
	var out []string
	for _, item := range rel.Data {
		a := attrs(item)
		if name := stringAttr(a, "name"); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func albumNameFromRelationship(r Resource) string {
	if album := FirstRelationshipResource(r, "albums"); album != nil {
		return stringAttr(attrs(*album), "name")
	}
	return ""
}

func NormalizeTrack(r Resource) model.Track {
	a := attrs(r)
	artist := stringAttr(a, "artistName")
	if names := artistNamesFromRelationship(r); len(names) > 0 {
		artist = strings.Join(names, ", ")
	}
	album := stringAttr(a, "albumName")
	if relAlbum := albumNameFromRelationship(r); relAlbum != "" {
		album = relAlbum
	}
	return model.Track{
		Type:       "track",
		ID:         catalogDisplayID(r),
		Kind:       KindFor(displayID(r), r.Type),
		Title:      fallback(stringAttr(a, "name"), displayID(r)),
		Artist:     artist,
		Album:      album,
		Duration:   intAttr(a, "durationInMillis") / 1000,
		TrackNo:    intAttr(a, "trackNumber"),
		DiscNo:     intAttr(a, "discNumber"),
		Genre:      genre(a),
		ArtworkURL: artwork(a, 600),
		PlayURL:    "/tracks/" + catalogDisplayID(r) + "/play",
		Raw:        r,
	}
}

func NormalizePlaylist(r Resource) model.Playlist {
	a := attrs(r)
	return model.Playlist{
		Type:        "playlist",
		ID:          displayID(r),
		Kind:        KindFor(displayID(r), r.Type),
		Name:        fallback(stringAttr(a, "name"), displayID(r)),
		Owner:       fallback(stringAttr(a, "curatorName"), "Apple Music"),
		TrackCount:  intAttr(a, "trackCount"),
		Description: description(a),
		ArtworkURL:  artwork(a, 600),
		CanEdit:     boolAttr(a, "canEdit"),
		Raw:         r,
	}
}

func NormalizeStation(r Resource, sourceTitle string) model.Playlist {
	a := r.Attributes
	return model.Playlist{
		Type:        "playlist",
		ID:          displayID(r),
		Kind:        model.KindStation,
		Name:        fallback(stringAttr(a, "name"), displayID(r)),
		Owner:       "Apple Music Radio",
		Description: description(a),
		ArtworkURL:  artwork(a, 600),
		Dynamic:     true,
		SourceTitle: sourceTitle,
		Raw:         r,
	}
}

func NormalizeAlbum(r Resource) model.Album {
	a := attrs(r)
	year := 0
	if release := stringAttr(a, "releaseDate"); len(release) >= 4 {
		fmt.Sscanf(release[:4], "%d", &year)
	}
	artist := stringAttr(a, "artistName")
	if names := artistNamesFromRelationship(r); len(names) > 0 {
		artist = strings.Join(names, ", ")
	}
	return model.Album{
		Type:        "album",
		ID:          catalogDisplayID(r),
		Kind:        KindFor(displayID(r), r.Type),
		Name:        fallback(stringAttr(a, "name"), displayID(r)),
		Artist:      artist,
		Year:        year,
		TrackCount:  intAttr(a, "trackCount"),
		Genre:       genre(a),
		Description: description(a),
		ArtworkURL:  artwork(a, 600),
		Raw:         r,
	}
}

func NormalizeArtist(r Resource) model.Artist {
	a := attrs(r)
	return model.Artist{
		Type:        "artist",
		ID:          catalogDisplayID(r),
		Kind:        KindFor(displayID(r), r.Type),
		Name:        fallback(stringAttr(a, "name"), displayID(r)),
		Genre:       genre(a),
		Description: description(a),
		ArtworkURL:  artwork(a, 600),
		Raw:         r,
	}
}

func fallback(value, def string) string {
	if value != "" {
		return value
	}
	return def
}
