package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseArchiveAndChecksums(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("release target is linux/amd64")
	}
	out := t.TempDir()
	cmd := exec.Command("bash", "scripts/package-release.sh", out)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package: %v\n%s", err, result)
	}
	archive := filepath.Join(out, "nvidia-nim_0.2.5_linux_amd64.zip")
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	checksums, err := os.ReadFile(filepath.Join(out, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(checksums), hex.EncodeToString(sum[:])+"  nvidia-nim_0.2.5_linux_amd64.zip") {
		t.Fatalf("incorrect checksums %s", checksums)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	found := false
	var libBytes []byte
	for _, f := range reader.File {
		if f.Name == "nvidia-nim.so" && f.UncompressedSize64 > 0 {
			found = true
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			libBytes, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		if strings.Contains(f.Name, "/") || strings.HasSuffix(f.Name, ".h") {
			t.Fatalf("unexpected archive entry %q", f.Name)
		}
	}
	if !found {
		t.Fatal("archive lacks nvidia-nim.so")
	}
	libSum := sha256.Sum256(libBytes)
	libChecksums, err := os.ReadFile(filepath.Join(out, "library.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(libChecksums), hex.EncodeToString(libSum[:])+"  nvidia-nim.so") {
		t.Fatalf("incorrect library checksum %s", libChecksums)
	}
}
