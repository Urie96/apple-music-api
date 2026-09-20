package main

import (
	"log"
	"net/http"

	widevine "github.com/iyear/gowidevine"

	"github.com/urie96/apple-music-api/internal/apple"
	"github.com/urie96/apple-music-api/internal/config"
	"github.com/urie96/apple-music-api/internal/httpapi"
	"github.com/urie96/apple-music-api/internal/playback"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	logCredentialGuidance(cfg)

	client := apple.NewClient(cfg.Credential)
	appleSvc := apple.NewService(client)

	device, err := playback.LoadWidevineDevice(cfg.WVDPath)
	if err != nil {
		log.Fatalf("load Widevine device: %v", err)
	}
	cdm := widevine.NewCDM(device)
	playbackSvc := playback.NewService(client, cfg.CacheDir, cfg.MaxCacheEntries, cdm)
	if err := playbackSvc.InitCache(); err != nil {
		log.Fatalf("init cache: %v", err)
	}

	server := httpapi.New(&cfg, client, appleSvc, playbackSvc)

	log.Printf("Apple Music API listening on http://%s", cfg.Addr())
	log.Printf("Credential store: %s (app=%s user=%s)", cfg.CredentialPath, cfg.Credential.AppTokenSource, cfg.Credential.UserTokenSource)
	log.Printf("Cache dir: %s (max entries %d)", cfg.CacheDir, cfg.MaxCacheEntries)
	log.Printf("WVD: %s", cfg.WVDPath)
	log.Fatal(http.ListenAndServe(cfg.Addr(), server.Handler()))
}

// logCredentialGuidance prints instructions on how to initialize Apple Music
// credentials when they are missing, so the user is not left with a service
// that silently fails every authenticated request.
func logCredentialGuidance(cfg config.Config) {
	if cfg.HasCredential() {
		return
	}

	log.Printf("WARNING: Apple Music credentials are not fully configured.")
	if cfg.Credential.AppToken == "" {
		log.Printf("  - Missing app token (MusicKit developer token)")
	}
	if cfg.Credential.UserToken == "" {
		log.Printf("  - Missing user token")
	}
	log.Printf("Initialize credentials with one of the following:")
	log.Printf("  1. Browser (recommended): open http://%s/auth/apple", cfg.Addr())
	log.Printf("     Paste your MusicKit developer token, click \"Initialize MusicKit\",")
	log.Printf("     then click \"Sign in to Apple Music\" and finish authorization.")
	log.Printf("     The user token is saved automatically to %s", cfg.CredentialPath)
	log.Printf("  2. Flags: ./apple-music-api --app-token \"<developer token>\" --user-token \"<user token>\"")
	log.Printf("  3. Env:   APPLE_MUSIC_APP_TOKEN=\"<developer token>\" APPLE_MUSIC_USER_TOKEN=\"<user token>\" ./apple-music-api")
	log.Printf("The service is still starting; authenticated endpoints will fail until credentials are set.")
}
