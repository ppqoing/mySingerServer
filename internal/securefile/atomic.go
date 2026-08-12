package securefile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrVerify = errors.New("secure_file_verify_failed")

type Loader func(string) ([]byte, error)

// WriteAtomic publishes data in the target directory after the closed temp
// file has been flushed, protected, and reloaded. The formal target is then
// reloaded so callers never accept a corrupted replacement as successful.
func WriteAtomic(target string, data []byte, loader Loader) (err error) {
	if target == "" || loader == nil {
		return errors.New("secure file unavailable")
	}
	directory := filepath.Dir(target)
	temp, err := createRestrictedTemp(directory, "."+filepath.Base(target)+".*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	validated, err := loader(tempPath)
	if err != nil || !bytes.Equal(validated, data) {
		if err != nil {
			return err
		}
		return errors.New("secure file temporary verification failed")
	}
	if err := atomicReplace(tempPath, target); err != nil {
		return err
	}
	formal, err := loader(target)
	if err != nil {
		return fmt.Errorf("%w: formal target invalid", ErrVerify)
	}
	if !bytes.Equal(formal, data) {
		return fmt.Errorf("%w: formal target mismatch", ErrVerify)
	}
	return syncDirectory(directory)
}
