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
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mp4 "github.com/Eyevinn/mp4ff/mp4"
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

	// File-based cache. Decrypted tracks stored as .mp4 files. Survives restarts,
	// uses negligible memory. Capped at startup, grows without bound during
	// a session (100 tracks ≈ 600 MB max, harmless).
	cacheDir string // set from -cache-dir flag

	// CDM is not thread-safe (c.rand races under concurrent use).
	cdmMu sync.Mutex
)

const (
	webPlaybackURL  = "https://play.music.apple.com/WebObjects/MZPlay.woa/wa/webPlayback"
	maxCacheEntries = 100 // max cached .mp4 files (~6 MB each → ~600 MB peak)
)

// Widevine System ID
var widevineSystemID, _ = hex.DecodeString(widevine.WidevineSystemID)

func cachePath(trackID string) string {
	return filepath.Join(cacheDir, trackID+".mp4")
}

// =============================================================================
// Main
// =============================================================================

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	port := flag.Int("port", 8899, "HTTP listen port")
	wvdPath := flag.String("wvd", "oneplus_pjx110_18.0.0@341310000_9b731721_28613_l3.wvd", "path to .wvd device file")
	cacheFlag := flag.String("cache-dir",
		filepath.Join(os.TempDir(), "apple-music-proxy-cache"),
		"cache directory for decrypted tracks (cleared on startup)")
	flag.Parse()
	cacheDir = *cacheFlag

	if appToken == "" {
		log.Fatal("APPLE_MUSIC_APP_TOKEN is required")
	}
	if userToken == "" {
		log.Fatal("APPLE_MUSIC_USER_TOKEN is required")
	}

	// Init cache directory (reuse files from previous runs, trim to cap)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Fatalf("create cache dir: %v", err)
	}
	entries, _ := os.ReadDir(cacheDir)
	type cf struct {
		name    string
		modTime int64
	}
	var files []cf
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".mp4") {
			fi, err := e.Info()
			if err != nil {
				continue
			}
			files = append(files, cf{name: e.Name(), modTime: fi.ModTime().UnixNano()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime < files[j].modTime })
	for i := 0; i < len(files)-maxCacheEntries; i++ {
		os.Remove(filepath.Join(cacheDir, files[i].name))
	}
	log.Printf("Cache dir: %s (%d files, max %d)", cacheDir, min(len(files), maxCacheEntries), maxCacheEntries)

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

		// 1. Library track → always need webPlayback for redirect URL.
		if isLibraryID(trackID) {
			song, err := fetchSongMetadata(trackID)
			if err != nil {
				http.Error(w, fmt.Sprintf("webPlayback: %v", err), http.StatusBadGateway)
				return
			}
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

		// 2. Catalog track → check file cache BEFORE hitting Apple.
		if cachedPath := cachePath(trackID); fileExists(cachedPath) {
			http.ServeFile(w, r, cachedPath)
			return
		}

		// 3. Cache miss → webPlayback to get m3u8 URL.
		song, err := fetchSongMetadata(trackID)
		if err != nil {
			http.Error(w, fmt.Sprintf("webPlayback: %v", err), http.StatusBadGateway)
			return
		}

		// 4. Catalog track → find ctrp256 m3u8
		m3u8URL, licenseURL, skdURI, keyID := extractM3U8Info(song)
		if m3u8URL == "" {
			http.Error(w, "no ctrp256 asset found", http.StatusNotFound)
			return
		}

		log.Printf("[catalog] %s m3u8=%s", trackID, m3u8URL[:min(len(m3u8URL), 80)])
		log.Printf("[catalog] %s skd=%s keyID=%x", trackID, skdURI[:min(len(skdURI), 80)], keyID)

		// 5. Widevine license → content key (hex)
		keyHex, err := getWidevineKey(cdm, keyID, licenseURL, skdURI, trackID)
		if err != nil {
			log.Printf("[catalog] %s Widevine FAILED: %v", trackID, err)
			http.Error(w, fmt.Sprintf("Widevine: %v", err), http.StatusBadGateway)
			return
		}
		log.Printf("[catalog] %s → key obtained (%d hex chars)", trackID, len(keyHex))

		// 6. Download & decrypt segments, stream to mpv.
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

	cdmMu.Lock()
	challenge, parseLicense, err := cdm.GetLicenseChallenge(
		pssh,
		widevinepb.LicenseType_STREAMING,
		false, // privacyMode=false
	)
	cdmMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("license challenge: %w", err)
	}

	// Apple's license server expects JSON, not protobuf.
	license, err := requestAppleLicense(licenseURL, skdURI, challenge, trackID)
	if err != nil {
		return "", fmt.Errorf("request license: %w", err)
	}

	cdmMu.Lock()
	keys, err := parseLicense(license)
	cdmMu.Unlock()
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
// Per-segment HLS streaming (download → decrypt → flush, one segment at a time)
// =============================================================================

type hlsSegment struct {
	uri       string
	byteRange string // e.g. "369773@1247" or "" for full file
	isInit    bool
}

func streamTrack(w http.ResponseWriter, r *http.Request, m3u8URL, keyHex, trackID string) {
	t0 := time.Now()
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

	// 1. Parse HLS playlist
	base := m3u8URL[:strings.LastIndexByte(m3u8URL, '/')+1]
	segs, err := parseHLSSegments(m3u8URL, base)
	if err != nil {
		http.Error(w, fmt.Sprintf("parse HLS: %v", err), http.StatusBadGateway)
		return
	}
	if len(segs) == 0 || !segs[0].isInit {
		http.Error(w, "no init segment in HLS playlist", http.StatusBadGateway)
		return
	}

	// 2. Download & parse init segment to extract decrypt info
	initBytes, err := downloadSegmentBytes(segs[0])
	if err != nil {
		http.Error(w, fmt.Sprintf("download init: %v", err), http.StatusBadGateway)
		return
	}
	initFile, err := mp4.DecodeFile(bytes.NewReader(initBytes))
	if err != nil {
		http.Error(w, fmt.Sprintf("parse init: %v", err), http.StatusInternalServerError)
		return
	}
	decryptInfo, err := mp4.DecryptInit(initFile.Init)
	if err != nil {
		http.Error(w, fmt.Sprintf("decrypt init: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[catalog] %s streaming %d segments (%d media)...", trackID, len(segs), len(segs)-1)

	// 3. Set up chunked streaming response + temp file for cache
	w.Header().Set("Content-Type", "audio/mp4")
	w.Header().Set("Cache-Control", "no-cache")

	flusher, canFlush := w.(http.Flusher)

	tmpFile, err := os.CreateTemp(cacheDir, "tmp-*.mp4")
	if err != nil {
		http.Error(w, fmt.Sprintf("create temp file: %v", err), http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // clean up if we don't commit to cache

	writer := io.MultiWriter(w, tmpFile)

	// 4. Write decrypted init segment first
	if err := initFile.Init.Encode(writer); err != nil {
		log.Printf("[catalog] %s write init: %v", trackID, err)
		return
	}
	if canFlush {
		flusher.Flush()
	}

	// 5. Download, decrypt, and stream each media segment
	for i := 1; i < len(segs); i++ {
		segBytes, err := downloadSegmentBytes(segs[i])
		if err != nil {
			log.Printf("[catalog] %s segment %d download: %v", trackID, i, err)
			return
		}
		segFile, err := mp4.DecodeFile(bytes.NewReader(segBytes))
		if err != nil {
			log.Printf("[catalog] %s segment %d parse: %v", trackID, i, err)
			return
		}
		for _, seg := range segFile.Segments {
			if err := mp4.DecryptSegment(seg, decryptInfo, keyBytes); err != nil {
				if err.Error() == "no senc box in traf" {
					continue // unencrypted segment
				}
				log.Printf("[catalog] %s segment %d decrypt: %v", trackID, i, err)
				return
			}
			if err := seg.Encode(writer); err != nil {
				log.Printf("[catalog] %s segment %d encode: %v", trackID, i, err)
				return
			}
		}
		if canFlush {
			flusher.Flush()
		}
	}

	tmpFile.Close()
	fi, _ := os.Stat(tmpPath)

	log.Printf("[catalog] %s streaming complete (%d bytes, total %s)", trackID, fi.Size(), time.Since(t0).Round(time.Millisecond))

	// Commit to file cache. If another request already cached this track,
	// the rename fails silently (our temp file gets cleaned up by defer).
	os.Rename(tmpPath, cachePath(trackID))
}

func parseHLSSegments(m3u8URL, base string) ([]hlsSegment, error) {
	resp, err := http.Get(m3u8URL)
	if err != nil {
		return nil, fmt.Errorf("download m3u8: %w", err)
	}
	defer resp.Body.Close()
	playlist, _ := io.ReadAll(resp.Body)

	var segs []hlsSegment

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
		segs = append(segs, hlsSegment{uri: uri, byteRange: br, isInit: true})
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
		segs = append(segs, hlsSegment{uri: uri, byteRange: currentBR})
		currentBR = ""
	}

	return segs, nil
}

func downloadSegmentBytes(seg hlsSegment) ([]byte, error) {
	req, _ := http.NewRequest("GET", seg.uri, nil)
	if seg.byteRange != "" {
		parts := strings.SplitN(seg.byteRange, "@", 2)
		if len(parts) == 2 {
			length, _ := strconv.Atoi(parts[0])
			offset, _ := strconv.Atoi(parts[1])
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
		}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", seg.uri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// =============================================================================
// Helpers
// =============================================================================

func isLibraryID(id string) bool {
	return regexp.MustCompile(`^[ailp]\.[a-zA-Z0-9]+$`).MatchString(id)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
