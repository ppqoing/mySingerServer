package localcontrol

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTokenPathUsesPortableDataDirectory(t *testing.T) {
	root := filepath.Join("portable", "compute")
	want := filepath.Join(root, "data", "local-control.token")
	if got := TokenPath(root); got != want {
		t.Fatalf("TokenPath(%q) = %q, want %q", root, got, want)
	}
}

func TestFileTokenStoreCreatesRandomBase64URLToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "local-control.token")
	token, err := (FileTokenStore{}).LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("token is not unpadded base64url: %v", err)
	}
	if len(decoded) != 32 {
		t.Fatalf("decoded token length = %d, want 32", len(decoded))
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(stored) != token {
		t.Fatalf("stored token differs from returned token")
	}
}

func TestFileTokenStoreConcurrentFirstCreateReturnsOneToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "local-control.token")
	const callers = 24
	tokens := make(chan string, callers)
	errors := make(chan error, callers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			token, err := (FileTokenStore{}).LoadOrCreate(path)
			tokens <- token
			errors <- err
		}()
	}
	close(start)
	group.Wait()
	close(tokens)
	close(errors)

	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent LoadOrCreate: %v", err)
		}
	}
	want := ""
	for token := range tokens {
		if want == "" {
			want = token
		}
		if token != want {
			t.Fatalf("concurrent tokens differ")
		}
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != want {
		t.Fatalf("stored token differs from concurrent result")
	}
}
