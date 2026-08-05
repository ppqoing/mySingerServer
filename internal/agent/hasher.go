package agent

import (
	"crypto/sha512"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const HashBlockSize = 4 << 20

type Hasher interface {
	HashFile(path string) (sha512Hex string, err error)
}

type GoHasher struct{}

func (GoHasher) HashFile(path string) (string, error) {
	file, err := os.Open(longPathPrefix(path))
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha512.New()
	if _, err := io.CopyBuffer(hash, file, make([]byte, HashBlockSize)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func longPathPrefix(path string) string {
	if len(path) < 248 || strings.HasPrefix(path, `\\?\`) {
		return path
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if strings.HasPrefix(absolute, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(absolute, `\\`)
	}
	return `\\?\` + absolute
}
