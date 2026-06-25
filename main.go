package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	widevine "github.com/iyear/gowidevine"
	"github.com/iyear/gowidevine/widevinepb"
	"google.golang.org/protobuf/proto"
)

// =============================================================================
// Config (env vars + CLI flags)
// =============================================================================

var (
	appToken  = os.Getenv("APPLE_MUSIC_APP_TOKEN")
	userToken = os.Getenv("APPLE_MUSIC_USER_TOKEN")

	// In-memory cache of decrypted tracks (trackID → decrypted MP4 bytes)
	trackCache   = map[string][]byte{}
	trackCacheMu sync.Mutex
)

const (
	webPlaybackURL = "https://play.music.apple.com/WebObjects/MZPlay.woa/wa/webPlayback"
)

// Widevine System ID
var widevineSystemID, _ = hex.DecodeString(widevine.WidevineSystemID)

// =============================================================================
// Main
// =============================================================================

func main() {
	port := flag.Int("port", 8899, "HTTP listen port")
	wvdPath := flag.String("wvd", "oneplus_pjx110_18.0.0@341310000_9b731721_28613_l3.wvd", "path to .wvd device file")
	flag.Parse()

	if appToken == "" {
		log.Fatal("APPLE_MUSIC_APP_TOKEN is required")
	}
	if userToken == "" {
		log.Fatal("APPLE_MUSIC_USER_TOKEN is required")
	}

	// Load Widevine L3 device once at startup.
	device, err := loadWidevineDevice(*wvdPath)
	if err != nil {
		log.Fatalf("load Widevine device: %v", err)
	}
	cdm := widevine.NewCDM(device)

	http.HandleFunc("/play", makePlayHandler(cdm))
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	log.Printf("Apple Music Proxy listening on http://127.0.0.1:%d", *port)
	log.Printf("  WVD: %s", *wvdPath)
	log.Printf("  Usage: mpv http://127.0.0.1:%d/play?track_id=TRACK_ID", *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), nil))
}

// =============================================================================
// HTTP handler
// =============================================================================

func makePlayHandler(cdm *widevine.CDM) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		trackID := strings.TrimSpace(r.URL.Query().Get("track_id"))
		if trackID == "" {
			http.Error(w, "missing track_id", http.StatusBadRequest)
			return
		}

		// 1. webPlayback → song metadata
		song, err := fetchSongMetadata(trackID)
		if err != nil {
			http.Error(w, fmt.Sprintf("webPlayback: %v", err), http.StatusBadGateway)
			return
		}

		// 2. Library track → redirect to direct asset URL
		if isLibraryID(trackID) {
			for _, a := range song.Assets {
				if a.URL != "" {
					log.Printf("[library] %s → 307", trackID)
					http.Redirect(w, r, a.URL, http.StatusTemporaryRedirect)
					return
				}
			}
			http.Error(w, "no playable asset for library track", http.StatusNotFound)
			return
		}

		// Check cache first (mpv retries, so second request should be instant).
		trackCacheMu.Lock()
		cached, hasCached := trackCache[trackID]
		trackCacheMu.Unlock()
		if hasCached {
			log.Printf("[catalog] %s serving from cache (%d bytes)", trackID, len(cached))
			serveData(w, r, cached)
			return
		}

		// 3. Catalog track → find ctrp256 m3u8
		m3u8URL, licenseURL, skdURI, keyID := extractM3U8Info(song)
		if m3u8URL == "" {
			http.Error(w, "no ctrp256 asset found", http.StatusNotFound)
			return
		}

		log.Printf("[catalog] %s m3u8=%s", trackID, m3u8URL[:min(len(m3u8URL), 80)])
		log.Printf("[catalog] %s skd=%s keyID=%x", trackID, skdURI[:min(len(skdURI), 80)], keyID)

		// 4. Widevine license → content key (hex)
		keyHex, err := getWidevineKey(cdm, keyID, licenseURL, skdURI, trackID)
		if err != nil {
			log.Printf("[catalog] %s Widevine FAILED: %v", trackID, err)
			http.Error(w, fmt.Sprintf("Widevine: %v", err), http.StatusBadGateway)
			return
		}
		log.Printf("[catalog] %s → key obtained (%d hex chars)", trackID, len(keyHex))

		// 5. Download segments, decrypt with gowidevine, stream to mpv.
		streamTrack(w, r, m3u8URL, keyHex, trackID)
	}
}

// =============================================================================
// webPlayback
// =============================================================================

type webPlaybackSong struct {
	Assets          []webPlaybackAsset `json:"assets"`
	HLSKeyServerURL string             `json:"hls-key-server-url"`
}

type webPlaybackAsset struct {
	URL    string `json:"URL"`
	Flavor string `json:"flavor"`
}

type webPlaybackResponse struct {
	SongList []webPlaybackSong `json:"songList"`
}

func fetchSongMetadata(trackID string) (*webPlaybackSong, error) {
	body := map[string]interface{}{}
	if isLibraryID(trackID) {
		body["universalLibraryId"] = trackID
		body["isLibrary"] = true
	} else {
		body["salableAdamId"] = trackID
	}
	jsonBody, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", webPlaybackURL, bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json;charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+appToken)
	req.Header.Set("Media-User-Token", userToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://music.apple.com")
	req.Header.Set("Referer", "https://music.apple.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result webPlaybackResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(result.SongList) == 0 {
		return nil, fmt.Errorf("empty songList")
	}
	return &result.SongList[0], nil
}

// =============================================================================
// HLS playlist parsing
// =============================================================================

func extractM3U8Info(song *webPlaybackSong) (m3u8URL, licenseURL, skdURI string, keyID []byte) {
	// ctrp256 asset = AES-128-CTR encrypted HLS
	for _, a := range song.Assets {
		if a.Flavor == "28:ctrp256" {
			m3u8URL = a.URL
			break
		}
	}
	if m3u8URL == "" {
		return "", "", "", nil
	}

	licenseURL = song.HLSKeyServerURL

	// Download m3u8 playlist to extract the skd:// URI.
	req, _ := http.NewRequest("GET", m3u8URL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("WARNING: download m3u8 failed: %v", err)
		return m3u8URL, licenseURL, "", nil
	}
	defer resp.Body.Close()
	playlist, _ := io.ReadAll(resp.Body)
	log.Printf("DEBUG: m3u8 downloaded: %d bytes, status=%d", len(playlist), resp.StatusCode)
	if len(playlist) > 0 && len(playlist) < 500 {
		log.Printf("DEBUG: m3u8 content: %s", string(playlist))
	}

	// Parse: URI="data:;base64,<PSSH>" or URI="skd://..."
	// With auth headers, Apple serves the PSSH base64-encoded in a data: URI.
	// Without auth (e.g. curl), it serves a 'skd://' URI path.
	re := regexp.MustCompile(`URI="(?:data:;base64,|skd://)([^"]+)"`)
	match := re.FindSubmatch(playlist)
	if len(match) < 2 {
		log.Printf("WARNING: no PSSH/URI in m3u8 playlist")
		return m3u8URL, licenseURL, "", nil
	}

	uri := string(match[1])

	// data:;base64 scheme → key_id is the base64-decoded payload
	var derr error
	keyID, derr = base64.StdEncoding.DecodeString(uri)
	if derr != nil {
		log.Printf("WARNING: base64 decode key_id failed: %v (data=%q)", derr, uri)
	}
	log.Printf("DEBUG: keyID from base64: len=%d hex=%x", len(keyID), keyID)
	if len(keyID) == 0 {
		// skd:// scheme → extract last UUID segment as key_id
		keyID = extractUUIDFromPath(uri)
	}

	// The key URI sent to Apple's license server (full URI from the playlist).
	if bytes.Contains(playlist, []byte("data:;base64,")) {
		skdURI = "data:;base64," + string(match[1])
	} else {
		skdURI = "skd://" + string(match[1])
	}

	return m3u8URL, licenseURL, skdURI, keyID
}

// extractUUIDFromPath returns the last hex-dehyphenated UUID segment embedded
// in a path like "itunes.apple.com/.../v4/c7/f3/8a/c7f38a00-a4e5-0de3-ecf6-c61b4cfcfdb5".
func extractUUIDFromPath(path string) []byte {
	// Match a standard UUID at the end of the path (with or without dashes).
	re := regexp.MustCompile(`([0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12})$`)
	m := re.FindStringSubmatch(path)
	if len(m) < 2 {
		return nil
	}
	raw, _ := hex.DecodeString(strings.ReplaceAll(m[1], "-", ""))
	return raw
}

// =============================================================================
// Widevine CDM
// =============================================================================

func loadWidevineDevice(path string) (*widevine.Device, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open wvd: %w", err)
	}
	defer f.Close()
	return widevine.NewDevice(widevine.FromWVD(f))
}

func buildPSSHFromKeyID(keyID []byte) ([]byte, error) {
	wvData := &widevinepb.WidevinePsshData{
		Algorithm: widevinepb.WidevinePsshData_AESCTR.Enum(),
		KeyIds:    [][]byte{keyID},
	}
	initData, err := proto.Marshal(wvData)
	if err != nil {
		return nil, fmt.Errorf("marshal WidevinePsshData: %w", err)
	}

	// Build full PSSH box (ISO/IEC 23001-7)
	buf := new(bytes.Buffer)
	// Box header: size (4) + type "pssh" (4) = 8
	// FullBox: version (1) + flags (3) = 4
	// systemID: 16
	// data size: 4
	// data: initData
	boxDataSize := 4 + 16 + 4 + len(initData)
	totalSize := 8 + boxDataSize

	binary.Write(buf, binary.BigEndian, uint32(totalSize))
	buf.WriteString("pssh")
	binary.Write(buf, binary.BigEndian, uint32(0)) // version=0, flags=0
	buf.Write(widevineSystemID)
	binary.Write(buf, binary.BigEndian, uint32(len(initData)))
	buf.Write(initData)

	return buf.Bytes(), nil
}

func getWidevineKey(cdm *widevine.CDM, keyID []byte, licenseURL, skdURI, trackID string) (string, error) {
	if len(keyID) == 0 {
		return "", fmt.Errorf("empty key_id – cannot build PSSH")
	}

	psshBytes, err := buildPSSHFromKeyID(keyID)
	if err != nil {
		return "", err
	}

	pssh, err := widevine.NewPSSH(psshBytes)
	if err != nil {
		return "", fmt.Errorf("parse PSSH: %w", err)
	}

	challenge, parseLicense, err := cdm.GetLicenseChallenge(
		pssh,
		widevinepb.LicenseType_STREAMING,
		false, // privacyMode=false
	)
	if err != nil {
		return "", fmt.Errorf("license challenge: %w", err)
	}

	// Apple's license server expects JSON, not protobuf.
	license, err := requestAppleLicense(licenseURL, skdURI, challenge, trackID)
	if err != nil {
		return "", fmt.Errorf("request license: %w", err)
	}

	keys, err := parseLicense(license)
	if err != nil {
		return "", fmt.Errorf("parse license: %w", err)
	}

	for _, k := range keys {
		if k.Type == widevinepb.License_KeyContainer_CONTENT {
			return hex.EncodeToString(k.Key), nil
		}
	}
	return "", fmt.Errorf("no CONTENT key in license response")
}

// requestAppleLicense sends the Widevine challenge to Apple's license server.
// Apple uses a custom JSON format, not the standard protobuf.
func requestAppleLicense(licenseURL, skdURI string, challenge []byte, trackID string) ([]byte, error) {
	body := map[string]interface{}{
		"challenge":      base64.StdEncoding.EncodeToString(challenge),
		"key-system":     "com.widevine.alpha",
		"uri":            skdURI,
		"adamId":         trackID,
		"isLibrary":      false,
		"user-initiated": true,
	}
	jsonBody, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", licenseURL, bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+appToken)
	req.Header.Set("Media-User-Token", userToken)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST license: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
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

// =============================================================================
// FFmpeg streaming
// =============================================================================

func streamTrack(w http.ResponseWriter, r *http.Request, m3u8URL, keyHex, trackID string) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[catalog] %s PANIC: %v", trackID, rec)
			http.Error(w, fmt.Sprintf("internal error: %v", rec), http.StatusInternalServerError)
		}
	}()
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid key: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[catalog] %s downloading & decrypting segments...", trackID)

	encrypted, err := downloadSegments(m3u8URL)
	if err != nil {
		http.Error(w, fmt.Sprintf("download segments: %v", err), http.StatusBadGateway)
		return
	}

	key := &widevine.Key{
		ID:   nil,
		Key:  keyBytes,
		Type: widevinepb.License_KeyContainer_CONTENT,
	}
	var decrypted bytes.Buffer
	if err := widevine.DecryptMP4Auto(bytes.NewReader(encrypted), []*widevine.Key{key}, &decrypted); err != nil {
		http.Error(w, fmt.Sprintf("decrypt MP4: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[catalog] %s decrypted %d → %d bytes", trackID, len(encrypted), decrypted.Len())

	// Cache for future requests
	trackCacheMu.Lock()
	trackCache[trackID] = decrypted.Bytes()
	trackCacheMu.Unlock()

	serveData(w, r, decrypted.Bytes())
}

func downloadSegments(m3u8URL string) ([]byte, error) {
	base := m3u8URL[:strings.LastIndexByte(m3u8URL, '/')+1]

	resp, err := http.Get(m3u8URL)
	if err != nil {
		return nil, fmt.Errorf("download m3u8: %w", err)
	}
	defer resp.Body.Close()
	playlist, _ := io.ReadAll(resp.Body)

	type seg struct {
		uri       string
		byteRange string // e.g. "369773@1247" or "" for full file
	}
	var segs []seg

	// Init segment from #EXT-X-MAP
	mapRe := regexp.MustCompile(`#EXT-X-MAP:URI="([^"]+)"(?:,BYTERANGE="([^"]+)")?`)
	if m := mapRe.FindSubmatch(playlist); len(m) >= 2 {
		uri := string(m[1])
		br := ""
		if len(m) >= 3 {
			br = string(m[2])
		}
		if !strings.HasPrefix(uri, "http") {
			uri = base + uri
		}
		segs = append(segs, seg{uri: uri, byteRange: br})
	}

	// Media segments: BYTERANGE on the EXT-X-BYTERANGE line, URI on the next line
	lines := strings.Split(string(playlist), "\n")
	var currentBR string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-BYTERANGE:") {
			currentBR = strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
			continue
		}
		if strings.HasPrefix(line, "#") {
			currentBR = ""
			continue
		}
		if line == "" {
			currentBR = ""
			continue
		}
		uri := line
		if !strings.HasPrefix(uri, "http") {
			uri = base + uri
		}
		segs = append(segs, seg{uri: uri, byteRange: currentBR})
		currentBR = ""
	}

	log.Printf("downloading %d HLS segments...", len(segs))
	var buf bytes.Buffer
	for _, s := range segs {
		req, _ := http.NewRequest("GET", s.uri, nil)
		if s.byteRange != "" {
			// HLS BYTERANGE format: "length@offset" → HTTP Range: "bytes=offset-(offset+length-1)"
			parts := strings.SplitN(s.byteRange, "@", 2)
			if len(parts) == 2 {
				length, _ := strconv.Atoi(parts[0])
				offset, _ := strconv.Atoi(parts[1])
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
			}
		}
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("download segment %s: %w", s.uri, err)
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			return nil, fmt.Errorf("download segment %s: HTTP %d", s.uri, resp.StatusCode)
		}
		io.Copy(&buf, resp.Body)
		resp.Body.Close()
	}
	return buf.Bytes(), nil
}

// =============================================================================
// Helpers
// =============================================================================

func isLibraryID(id string) bool {
	return regexp.MustCompile(`^[ailp]\.[a-zA-Z0-9]+$`).MatchString(id)
}

// serveData writes data with proper Range support for mpv seeking.
func serveData(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Type", "audio/mp4")
	w.Header().Set("Accept-Ranges", "bytes")

	rangeHdr := r.Header.Get("Range")
	if strings.HasPrefix(rangeHdr, "bytes=") {
		rangeVal := strings.TrimPrefix(rangeHdr, "bytes=")
		parts := strings.SplitN(rangeVal, "-", 2)
		var start, end int64
		start, _ = strconv.ParseInt(parts[0], 10, 64)
		if len(parts) > 1 && parts[1] != "" {
			end, _ = strconv.ParseInt(parts[1], 10, 64)
		} else {
			end = int64(len(data)) - 1
		}
		if start < 0 || end >= int64(len(data)) || start > end {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : end+1])
		return
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data)
}
