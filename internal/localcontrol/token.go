package localcontrol

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
)

const tokenBytes = 32

type TokenStore interface {
	LoadOrCreate(path string) (string, error)
}

type FileTokenStore struct{}

func TokenPath(portableRoot string) string {
	return filepath.Join(portableRoot, "data", "local-control.token")
}

func (FileTokenStore) LoadOrCreate(path string) (string, error) {
	candidateBytes := make([]byte, tokenBytes)
	if _, err := rand.Read(candidateBytes); err != nil {
		return "", fmt.Errorf("generate local control token: %w", err)
	}
	candidate := base64.RawURLEncoding.EncodeToString(candidateBytes)
	token, err := platformLoadOrCreate(path, candidate)
	if err != nil {
		return "", fmt.Errorf("load or create local control token: %w", err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != tokenBytes {
		return "", errors.New("local control token file is invalid")
	}
	return token, nil
}
