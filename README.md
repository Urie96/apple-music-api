# apple-music-api

A local HTTP API service that exposes Apple Music account, catalog, library, playback, and decryption capabilities to local clients.

Bound to `127.0.0.1` only — no remote access.

## Quick Start

```bash
go build -o apple-music-api .

# Run with defaults
./apple-music-api

# Or with custom options
./apple-music-api --port 8899 --credential-store ~/.config/apple-music-api/credentials.json
```

The service starts on `http://127.0.0.1:8899`. Open it in a browser to authorize.

## Authorization

Visit `http://127.0.0.1:8899` (or `/auth/apple`) to authorize Apple Music:

1. Paste your MusicKit developer token
2. Click **Initialize MusicKit**
3. Click **Sign in to Apple Music** and authorize in the popup
4. User token is saved automatically

Credentials are persisted to the credential store. You can also supply them directly:

```bash
# Via flags (takes precedence over store)
./apple-music-api --app-token "eyJhbG..." --user-token "..."

# Via env vars
export APPLE_MUSIC_APP_TOKEN="eyJhbG..."
export APPLE_MUSIC_USER_TOKEN="..."
./apple-music-api
```

**Precedence**: flags > environment variables > credential store file.

## API Reference

All JSON responses follow a standard envelope:

```json
{
  "data": { ... },
  "meta": { "limit": 25, "offset": 0, "count": 25 },
  "error": { "code": "...", "message": "...", "detail": ... }
}
```

### Health

```
GET /health
```

### Account & Storefront

```
GET /storefront
```

Returns the Apple Music storefront ID (e.g. `cn`, `us`).

### Library

```
GET /library/playlists            ?limit=&offset=
GET /library/playlists/:id/tracks ?limit=&offset=
POST /library/playlists/:id/tracks  {"track_id": "..."}
GET /library/albums               ?limit=&offset=
GET /library/albums/:id/tracks    ?limit=&offset=
GET /library/artists              ?limit=&offset=
GET /library/tracks               ?limit=&offset=
```

Add a track to a playlist with `POST`. Accepts both catalog and library track IDs.

### Favorites

```
GET /library/favorites/tracks     ?limit=&offset=
```

Returns only library tracks where `liked` is `true`.

### Ratings

```
PUT /ratings/tracks/:id  {"liked": true|false}
```

Set or unset the liked status on a track.

### Search

```
GET /search?term=...&types=songs,albums,artists,playlists&limit=&offset=
```

`types` defaults to `songs,albums,artists,playlists` if omitted.

### Catalog

```
GET /catalog/artists/:id/albums   ?limit=&offset=
```

Resolves library artist IDs to catalog IDs automatically.

### Recommendations & Stations

```
GET /recommendations              ?limit=&offset=
GET /stations/:id/tracks          ?limit=&offset=
```

Recommendations return personal recommendation content. Station IDs come from recommendation results.

### Playback

```
GET /tracks/:id/play
```

Returns a playable media stream (redirect or decrypted content). Uses local decrypted media cache to avoid repeated Widevine license requests.

## CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `8899` | HTTP listen port |
| `--credential-store` | `~/.config/apple-music-api/credentials.json` | Path to credential JSON file |
| `--app-token` | _empty_ | Apple Music developer token (overrides store) |
| `--user-token` | _empty_ | Apple Music user token (overrides store) |
| `--wvd` | `~/.config/apple-music-api/device.wvd` | Path to Widevine `.wvd` device file |
| `--cache-dir` | OS temp dir | Cache directory for decrypted tracks |
| `--max-cache-entries` | `100` | Maximum decrypted media cache entries |

## Environment Variables

| Variable | Description |
|----------|-------------|
| `APPLE_MUSIC_APP_TOKEN` | Developer token override |
| `APPLE_MUSIC_USER_TOKEN` | User token override |

## Requirements

- Go 1.26+
- A Widevine `.wvd` device file (for playback/decryption)
- An Apple Music developer token (MusicKit app token)

## License

MIT
