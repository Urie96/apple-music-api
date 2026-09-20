package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urie96/apple-music-api/internal/store"
)

type Source string

const (
	SourceEnv   Source = "env"
	SourceFlag  Source = "flag"
	SourceStore Source = "store"
	SourceEmpty Source = "empty"
)

type Credential struct {
	AppToken           string
	UserToken          string
	UserTokenUpdatedAt int64
	AppTokenSource     Source
	UserTokenSource    Source
}

type Config struct {
	Port            int
	ConfigDir       string
	WVDPath         string
	CacheDir        string
	MaxCacheEntries int
	CredentialPath  string
	Credential      Credential
	Store           *store.CredentialStore
}

func defaultConfigDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "apple-music-api")
	}
	return filepath.Join(os.TempDir(), "apple-music-api")
}

func defaultCredentialPath(configDir string) string {
	return filepath.Join(configDir, "credentials.json")
}

func defaultWVDPath(configDir string) string {
	return filepath.Join(configDir, "device.wvd")
}

func Load() (Config, error) {
	var cfg Config
	appTokenFlag := flag.String("app-token", "", "Apple Music app/developer token (overrides credential store)")
	userTokenFlag := flag.String("user-token", "", "Apple Music user token (overrides credential store)")
	configDirFlag := flag.String("config-dir", "", "base directory for config files (default "+defaultConfigDir()+")")
	wvdFlag := flag.String("wvd", "", "path to .wvd device file (default <config-dir>/device.wvd)")
	credentialPathFlag := flag.String("credential-store", "", "path to credential store JSON file (default <config-dir>/credentials.json)")
	flag.IntVar(&cfg.Port, "port", 8899, "HTTP listen port")
	flag.StringVar(&cfg.CacheDir, "cache-dir", filepath.Join(os.TempDir(), "apple-music-api-cache"), "cache directory for decrypted tracks")
	flag.IntVar(&cfg.MaxCacheEntries, "max-cache-entries", 100, "maximum decrypted media cache entries")
	flag.Parse()

	cfg.ConfigDir = strings.TrimSpace(*configDirFlag)
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = defaultConfigDir()
	}

	cfg.WVDPath = strings.TrimSpace(*wvdFlag)
	if cfg.WVDPath == "" {
		cfg.WVDPath = defaultWVDPath(cfg.ConfigDir)
	}

	cfg.CredentialPath = strings.TrimSpace(*credentialPathFlag)
	if cfg.CredentialPath == "" {
		cfg.CredentialPath = defaultCredentialPath(cfg.ConfigDir)
	}

	cfg.Store = store.NewCredentialStore(cfg.CredentialPath)
	stored, _ := cfg.Store.Load()

	cfg.Credential.AppToken, cfg.Credential.AppTokenSource = chooseCredential(
		*appTokenFlag,
		os.Getenv("APPLE_MUSIC_APP_TOKEN"),
		stored.AppToken,
	)
	cfg.Credential.UserToken, cfg.Credential.UserTokenSource = chooseCredential(
		*userTokenFlag,
		os.Getenv("APPLE_MUSIC_USER_TOKEN"),
		stored.UserToken,
	)
	cfg.Credential.UserTokenUpdatedAt = stored.UserTokenUpdatedAt
	return cfg, nil
}

func chooseCredential(flagValue, envValue, storeValue string) (string, Source) {
	if strings.TrimSpace(flagValue) != "" {
		return strings.TrimSpace(flagValue), SourceFlag
	}
	if strings.TrimSpace(envValue) != "" {
		return strings.TrimSpace(envValue), SourceEnv
	}
	if strings.TrimSpace(storeValue) != "" {
		return strings.TrimSpace(storeValue), SourceStore
	}
	return "", SourceEmpty
}

func (c Config) Addr() string { return fmt.Sprintf("127.0.0.1:%d", c.Port) }

func (c Config) HasCredential() bool {
	return c.Credential.AppToken != "" && c.Credential.UserToken != ""
}

func (c Config) EnvUserTokenOverridesStore() bool {
	return c.Credential.UserTokenSource == SourceEnv || c.Credential.UserTokenSource == SourceFlag
}
