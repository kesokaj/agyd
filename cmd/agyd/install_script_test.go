package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallScript(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("install.sh supports darwin and linux")
	}
	if runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Skip("install.sh supports arm64 and amd64")
	}

	scriptPath, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("bash -n install.sh failed: %v\n%s", err, out)
	}

	archiveName := fmt.Sprintf("agyd_0.2.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	payload := "#!/usr/bin/env bash\necho agyd-stub-ok\n"

	makeFixture := func(t *testing.T, archiveBytes []byte, checksumLine string, latestTag string) (string, string) {
		t.Helper()
		fixtureDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(fixtureDir, archiveName), archiveBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixtureDir, "checksums.txt"), []byte(checksumLine), 0o600); err != nil {
			t.Fatal(err)
		}

		stubBin := t.TempDir()
		stubCurl := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
out=""
write_url=0
url=""
while (( $# > 0 )); do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -w) write_url=1; shift 2 ;;
    --proto|--tlsv1.2|-fsSL) shift ;;
    --proto=*) shift ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
if [[ "$url" == *"/releases/latest" ]]; then
  if (( write_url )); then
    printf '%%s' %q
  fi
  exit 0
fi
base="$(basename "$url")"
src=%q"/$base"
[[ -f "$src" ]] || exit 22
cp "$src" "$out"
`, latestTag, fixtureDir)
		curlPath := filepath.Join(stubBin, "curl")
		if err := os.WriteFile(curlPath, []byte(stubCurl), 0o755); err != nil {
			t.Fatal(err)
		}
		return stubBin, fixtureDir
	}

	runInstall := func(t *testing.T, stubBin string, args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("bash", append([]string{scriptPath}, args...)...)
		cmd.Env = []string{
			"HOME=" + t.TempDir(),
			"PATH=" + stubBin + ":" + os.Getenv("PATH"),
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	goodArchive := buildTarGz(t, []tarEntry{{name: "agyd", mode: 0o755, body: payload}})
	sum := sha256.Sum256(goodArchive)
	goodHash := hex.EncodeToString(sum[:])
	goodChecksums := fmt.Sprintf("%s  %s\n", goodHash, archiveName)

	t.Run("installs latest release and verifies checksum", func(t *testing.T) {
		stubBin, _ := makeFixture(t, goodArchive, goodChecksums, "https://github.com/kesokaj/agyd/releases/tag/v0.2.0")
		installDir := filepath.Join(t.TempDir(), "bin")

		out, err := runInstall(t, stubBin, "--dir", installDir)
		if err != nil {
			t.Fatalf("install.sh failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Installed agyd v0.2.0") {
			t.Errorf("unexpected output:\n%s", out)
		}
		bin := filepath.Join(installDir, "agyd")
		got, err := exec.Command(bin).CombinedOutput()
		if err != nil || strings.TrimSpace(string(got)) != "agyd-stub-ok" {
			t.Fatalf("installed binary output = %q, err = %v", got, err)
		}
	})

	t.Run("refuses checksum mismatch without installing", func(t *testing.T) {
		badChecksums := fmt.Sprintf("%s  %s\n", strings.Repeat("0", 64), archiveName)
		stubBin, _ := makeFixture(t, goodArchive, badChecksums, "https://github.com/kesokaj/agyd/releases/tag/v0.2.0")
		installDir := filepath.Join(t.TempDir(), "bin")

		out, err := runInstall(t, stubBin, "--version", "v0.2.0", "--dir", installDir)
		if err == nil {
			t.Fatalf("expected failure on checksum mismatch; output:\n%s", out)
		}
		if !strings.Contains(out, "checksum mismatch") {
			t.Errorf("expected checksum mismatch message, got:\n%s", out)
		}
		if _, statErr := os.Stat(filepath.Join(installDir, "agyd")); !os.IsNotExist(statErr) {
			t.Errorf("tampered binary should not be installed, stat err = %v", statErr)
		}
	})

	t.Run("refuses missing checksum entry", func(t *testing.T) {
		stubBin, _ := makeFixture(t, goodArchive, fmt.Sprintf("%s  other.tar.gz\n", goodHash), "https://github.com/kesokaj/agyd/releases/tag/v0.2.0")
		installDir := filepath.Join(t.TempDir(), "bin")

		out, err := runInstall(t, stubBin, "--version", "v0.2.0", "--dir", installDir)
		if err == nil || !strings.Contains(out, "expected 1 checksum entry") {
			t.Fatalf("expected missing checksum failure, err=%v out=%s", err, out)
		}
	})

	t.Run("refuses symlink binary inside archive", func(t *testing.T) {
		symlinkArchive := buildTarGz(t, []tarEntry{{name: "agyd", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}})
		s := sha256.Sum256(symlinkArchive)
		checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(s[:]), archiveName)
		stubBin, _ := makeFixture(t, symlinkArchive, checksums, "https://github.com/kesokaj/agyd/releases/tag/v0.2.0")
		installDir := filepath.Join(t.TempDir(), "bin")

		out, err := runInstall(t, stubBin, "--version", "v0.2.0", "--dir", installDir)
		if err == nil || !strings.Contains(out, "regular agyd binary") {
			t.Fatalf("expected symlink refusal, err=%v out=%s", err, out)
		}
	})

	t.Run("rejects invalid version, repo, and unexpected redirect", func(t *testing.T) {
		stubBin, _ := makeFixture(t, goodArchive, goodChecksums, "https://github.com/kesokaj/agyd/releases")
		installDir := filepath.Join(t.TempDir(), "bin")

		for _, args := range [][]string{
			{"--version", "../evil", "--dir", installDir},
			{"--version", "v0.2.0-a..b", "--dir", installDir},
			{"--repo", "bad/repo;id", "--dir", installDir},
			{"--repo", "../..", "--dir", installDir},
			{"--repo", "ok/a..b", "--dir", installDir},
			{"--dir", installDir}, // latest redirects to /releases (no tag)
		} {
			if out, err := runInstall(t, stubBin, args...); err == nil {
				t.Errorf("args %v unexpectedly succeeded:\n%s", args, out)
			}
		}
	})
}

type tarEntry struct {
	name     string
	mode     int64
	body     string
	typeflag byte
	linkname string
}

func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf strings.Builder
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Size:     int64(len(e.body)),
			Typeflag: tf,
			Linkname: e.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if tf == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(buf.String())
}
