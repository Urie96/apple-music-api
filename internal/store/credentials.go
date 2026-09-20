package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type AppleCredential struct {
	AppToken           string    `json:"app_token"`
	UserToken          string    `json:"user_token"`
	UserTokenUpdatedAt int64     `json:"user_token_updated_at,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type CredentialStore struct {
	Path string
}

func NewCredentialStore(path string) *CredentialStore { return &CredentialStore{Path: path} }

func (s *CredentialStore) Load() (AppleCredential, error) {
	if strings.TrimSpace(s.Path) == "" {
		return AppleCredential{}, os.ErrNotExist
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return AppleCredential{}, err
	}
	var cred AppleCredential
	if err := json.Unmarshal(data, &cred); err != nil {
		return AppleCredential{}, fmt.Errorf("decode credential store: %w", err)
	}
	return cred, nil
}

func (s *CredentialStore) Save(cred AppleCredential) error {
	if strings.TrimSpace(s.Path) == "" {
		return errors.New("credential store path is empty")
	}
	cred.UpdatedAt = time.Now()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("create credential store dir: %w", err)
	}
	data, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credential store: %w", err)
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write credential store: %w", err)
	}
	return os.Rename(tmp, s.Path)
}
