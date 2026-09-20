package playback

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
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

	"github.com/urie96/apple-music-api/internal/apple"
)

var widevineSystemID, _ = hex.DecodeString(widevine.WidevineSystemID)

type Service struct {
	Apple           *apple.Client
	CacheDir        string
	MaxCacheEntries int
	CDM             *widevine.CDM
	cdmMu           sync.Mutex
}

func NewService(client *apple.Client, cacheDir string, maxCacheEntries int, cdm *widevine.CDM) *Service {
	return &Service{Apple: client, CacheDir: cacheDir, MaxCacheEntries: maxCacheEntries, CDM: cdm}
}

func LoadWidevineDevice(path string) (*widevine.Device, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open wvd: %w", err)
	}
	defer f.Close()
	return widevine.NewDevice(widevine.FromWVD(f))
}

func (s *Service) InitCache() error {
	if err := os.MkdirAll(s.CacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	return s.trimCache()
}

func (s *Service) trimCache() error {
	if s.MaxCacheEntries <= 0 {
		return nil
	}
	entries, err := os.ReadDir(s.CacheDir)
	if err != nil {
		return nil
	}
	type cf struct {
		name    string
		modTime int64
	}
	var files []cf
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".mp4") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, cf{name: e.Name(), modTime: fi.ModTime().UnixNano()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime < files[j].modTime })
	for i := 0; i < len(files)-s.MaxCacheEntries; i++ {
		_ = os.Remove(filepath.Join(s.CacheDir, files[i].name))
	}
	return nil
}

func (s *Service) cachePath(trackID string) string {
	return filepath.Join(s.CacheDir, safeCacheName(trackID)+".mp4")
}

func safeCacheName(id string) string {
	return regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(id, "_")
}

// resolveCatalogID looks up the catalog track ID for a library track.
// Returns empty string if no catalog mapping exists (e.g. user-uploaded track).
func (s *Service) resolveCatalogID(libraryID string) (string, error) {
	resp, err := s.Apple.List("me/library/songs/"+libraryID, map[string][]string{"include": {"catalog"}})
	if err != nil {
		return "", err
	}
	if len(resp.Data) == 0 {
		return "", fmt.Errorf("library track %s not found", libraryID)
	}
	if cat := apple.FirstRelationshipResource(resp.Data[0], "catalog"); cat != nil && cat.ID != "" {
		return cat.ID, nil
	}
	return "", nil
}

func (s *Service) ServeTrack(w http.ResponseWriter, r *http.Request, trackID string) {
	trackID = strings.TrimSpace(trackID)
	if trackID == "" {
		http.Error(w, "missing track id", http.StatusBadRequest)
		return
	}
	// Library tracks: resolve to catalog ID so Widevine decryption can work.
	if apple.IsLibraryID(trackID) {
		if catalogID, err := s.resolveCatalogID(trackID); err == nil && catalogID != "" {
			log.Printf("[library] %s → catalog %s", trackID, catalogID)
			trackID = catalogID
		} else {
			song, err := s.Apple.WebPlayback(trackID, true)
			if err != nil {
				http.Error(w, fmt.Sprintf("webPlayback: %v", err), http.StatusBadGateway)
				return
			}
			for _, a := range song.Assets {
				if a.URL != "" {
					http.Redirect(w, r, a.URL, http.StatusTemporaryRedirect)
					return
				}
			}
			http.Error(w, "no playable asset for library track", http.StatusNotFound)
			return
		}
	}

	if cachedPath := s.cachePath(trackID); fileExists(cachedPath) {
		http.ServeFile(w, r, cachedPath)
		return
	}

	song, err := s.Apple.WebPlayback(trackID, false)
	if err != nil {
		http.Error(w, fmt.Sprintf("webPlayback: %v", err), http.StatusBadGateway)
		return
	}
	m3u8URL, licenseURL, skdURI, keyID := extractM3U8Info(song)
	if m3u8URL == "" {
		http.Error(w, "no ctrp256 asset found", http.StatusNotFound)
		return
	}
	keyHex, err := s.getWidevineKey(keyID, licenseURL, skdURI, trackID)
	if err != nil {
		log.Printf("[catalog] %s Widevine FAILED: %v", trackID, err)
		http.Error(w, fmt.Sprintf("Widevine: %v", err), http.StatusBadGateway)
		return
	}
	s.streamTrack(w, r, m3u8URL, keyHex, trackID)
}

func extractM3U8Info(song *apple.WebPlaybackSong) (m3u8URL, licenseURL, skdURI string, keyID []byte) {
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
	req, _ := http.NewRequest("GET", m3u8URL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("WARNING: download m3u8 failed: %v", err)
		return m3u8URL, licenseURL, "", nil
	}
	defer resp.Body.Close()
	playlist, _ := io.ReadAll(resp.Body)
	re := regexp.MustCompile(`URI="(?:data:;base64,|skd://)([^"]+)"`)
	match := re.FindSubmatch(playlist)
	if len(match) < 2 {
		log.Printf("WARNING: no PSSH/URI in m3u8 playlist")
		return m3u8URL, licenseURL, "", nil
	}
	uri := string(match[1])
	keyID, _ = base64.StdEncoding.DecodeString(uri)
	if len(keyID) == 0 {
		keyID = extractUUIDFromPath(uri)
	}
	if bytes.Contains(playlist, []byte("data:;base64,")) {
		skdURI = "data:;base64," + string(match[1])
	} else {
		skdURI = "skd://" + string(match[1])
	}
	return m3u8URL, licenseURL, skdURI, keyID
}

func extractUUIDFromPath(path string) []byte {
	re := regexp.MustCompile(`([0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12})$`)
	m := re.FindStringSubmatch(path)
	if len(m) < 2 {
		return nil
	}
	raw, _ := hex.DecodeString(strings.ReplaceAll(m[1], "-", ""))
	return raw
}

func buildPSSHFromKeyID(keyID []byte) ([]byte, error) {
	wvData := &widevinepb.WidevinePsshData{Algorithm: widevinepb.WidevinePsshData_AESCTR.Enum(), KeyIds: [][]byte{keyID}}
	initData, err := proto.Marshal(wvData)
	if err != nil {
		return nil, fmt.Errorf("marshal WidevinePsshData: %w", err)
	}
	buf := new(bytes.Buffer)
	boxDataSize := 4 + 16 + 4 + len(initData)
	totalSize := 8 + boxDataSize
	_ = binary.Write(buf, binary.BigEndian, uint32(totalSize))
	buf.WriteString("pssh")
	_ = binary.Write(buf, binary.BigEndian, uint32(0))
	buf.Write(widevineSystemID)
	_ = binary.Write(buf, binary.BigEndian, uint32(len(initData)))
	buf.Write(initData)
	return buf.Bytes(), nil
}

func (s *Service) getWidevineKey(keyID []byte, licenseURL, skdURI, trackID string) (string, error) {
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
	s.cdmMu.Lock()
	challenge, parseLicense, err := s.CDM.GetLicenseChallenge(pssh, widevinepb.LicenseType_STREAMING, false)
	s.cdmMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("license challenge: %w", err)
	}
	license, err := s.Apple.RequestAppleLicense(licenseURL, skdURI, challenge, trackID)
	if err != nil {
		return "", fmt.Errorf("request license: %w", err)
	}
	s.cdmMu.Lock()
	keys, err := parseLicense(license)
	s.cdmMu.Unlock()
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

type hlsSegment struct {
	uri       string
	byteRange string
	isInit    bool
}

func (s *Service) streamTrack(w http.ResponseWriter, r *http.Request, m3u8URL, keyHex, trackID string) {
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid key: %v", err), http.StatusInternalServerError)
		return
	}
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
	w.Header().Set("Content-Type", "audio/mp4")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, canFlush := w.(http.Flusher)
	tmpFile, err := os.CreateTemp(s.CacheDir, "tmp-*.mp4")
	if err != nil {
		http.Error(w, fmt.Sprintf("create temp file: %v", err), http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	writer := io.MultiWriter(w, tmpFile)
	if err := initFile.Init.Encode(writer); err != nil {
		return
	}
	if canFlush {
		flusher.Flush()
	}
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
					continue
				}
				log.Printf("[catalog] %s segment %d decrypt: %v", trackID, i, err)
				return
			}
			if err := seg.Encode(writer); err != nil {
				return
			}
		}
		if canFlush {
			flusher.Flush()
		}
	}
	_ = tmpFile.Close()
	_ = os.Rename(tmpPath, s.cachePath(trackID))
	_ = s.trimCache()
}

func parseHLSSegments(m3u8URL, base string) ([]hlsSegment, error) {
	resp, err := http.Get(m3u8URL)
	if err != nil {
		return nil, fmt.Errorf("download m3u8: %w", err)
	}
	defer resp.Body.Close()
	playlist, _ := io.ReadAll(resp.Body)
	var segs []hlsSegment
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
	lines := strings.Split(string(playlist), "\n")
	var currentBR string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-BYTERANGE:") {
			currentBR = strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
			continue
		}
		if strings.HasPrefix(line, "#") || line == "" {
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
