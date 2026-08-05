package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func TestMain(tests *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__upgrade-cleanup" {
		os.Exit(0)
	}
	os.Exit(tests.Run())
}

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestRunUpgradesVerifiedBinaryAndRefreshesSkills(t *testing.T) {
	newBinary := replacementBinary(t, []byte("new roast binary"))
	archive := tarGzip(t, "roast", newBinary)
	digest := sha256.Sum256(archive)
	checksums := hex.EncodeToString(digest[:]) + "  roast_0.2.4_linux_arm64.tar.gz\n"
	metadata := `{"tag_name":"v0.2.4","assets":[` +
		`{"name":"roast_0.2.4_linux_arm64.tar.gz","browser_download_url":"https://downloads.test/archive"},` +
		`{"name":"checksums.txt","browser_download_url":"https://downloads.test/checksums"}]}`
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		content := metadata
		switch request.URL.String() {
		case "https://downloads.test/archive":
			return response(http.StatusOK, archive), nil
		case "https://downloads.test/checksums":
			content = checksums
		}
		return response(http.StatusOK, []byte(content)), nil
	})}
	destination := filepath.Join(t.TempDir(), testBinaryName())
	if err := os.WriteFile(destination, []byte("old roast binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	verified := false
	refreshed := false
	var output bytes.Buffer
	err := run(context.Background(), "0.2.3", &output, options{
		client:          client,
		executable:      func() (string, error) { return destination, nil },
		operatingSystem: "linux",
		architecture:    "arm64",
		verifyBinary: func(_ context.Context, path, expectedVersion string) error {
			verified = true
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if !bytes.Equal(content, newBinary) || expectedVersion != "0.2.4" {
				t.Fatalf("staged content = %q, version = %q", content, expectedVersion)
			}
			return nil
		},
		installSkills: func(_ context.Context, executable string, writer io.Writer) error {
			refreshed = true
			content, readErr := os.ReadFile(executable)
			if readErr != nil {
				return readErr
			}
			if !bytes.Equal(content, newBinary) {
				t.Fatalf("installed content = %q", content)
			}
			_, writeErr := io.WriteString(writer, "roast: Codex skill updated at /home/test/.codex/skills/roast/SKILL.md\n")
			return writeErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 3 || !verified || !refreshed {
		t.Fatalf("requests = %d, verified = %t, refreshed = %t", requests, verified, refreshed)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(destination)
		if err != nil {
			t.Fatal(err)
		}
		if permissions := info.Mode().Perm(); permissions != 0o700 {
			t.Fatalf("permissions = %o, want 700", permissions)
		}
	}
	for _, expected := range []string{"version: 0.2.3 -> 0.2.4", "binary updated at", "Codex skill updated at"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output = %q, missing %q", output.String(), expected)
		}
	}
}

func TestRunAlreadyUpToDateStillForceRefreshesSkills(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "roast")
	if err := os.WriteFile(destination, []byte("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	requests := 0
	refreshed := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return response(http.StatusOK, []byte(`{"tag_name":"v0.2.3","assets":[]}`)), nil
	})}
	var output bytes.Buffer
	err := run(context.Background(), "v0.2.3", &output, options{
		client:          client,
		executable:      func() (string, error) { return destination, nil },
		operatingSystem: "linux",
		architecture:    "arm64",
		installSkills: func(_ context.Context, executable string, _ io.Writer) error {
			refreshed = true
			resolved, resolveErr := filepath.EvalSymlinks(destination)
			if resolveErr != nil {
				return resolveErr
			}
			if executable != resolved {
				t.Fatalf("skill executable = %q, want %q", executable, resolved)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || !refreshed {
		t.Fatalf("requests = %d, refreshed = %t", requests, refreshed)
	}
	if !strings.Contains(output.String(), "already up to date at 0.2.3") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunKeepsNewerSemanticVersionAndStillRefreshesSkills(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "roast")
	if err := os.WriteFile(destination, []byte("release candidate"), 0o755); err != nil {
		t.Fatal(err)
	}
	refreshed := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, []byte(`{"tag_name":"v0.2.4","assets":[]}`)), nil
	})}
	var output bytes.Buffer
	err := run(context.Background(), "v0.3.0-rc.1", &output, options{
		client:          client,
		executable:      func() (string, error) { return destination, nil },
		operatingSystem: "linux",
		architecture:    "arm64",
		installSkills: func(context.Context, string, io.Writer) error {
			refreshed = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed || !strings.Contains(output.String(), "newer than latest release 0.2.4; binary kept") {
		t.Fatalf("refreshed = %t, output = %q", refreshed, output.String())
	}
	content, readErr := os.ReadFile(destination)
	if readErr != nil || string(content) != "release candidate" {
		t.Fatalf("content = %q, error = %v", content, readErr)
	}
}

func TestRunRejectsChecksumMismatchWithoutChangingBinary(t *testing.T) {
	archive := tarGzip(t, "roast", []byte("untrusted"))
	metadata := `{"tag_name":"v0.2.4","assets":[` +
		`{"name":"roast_0.2.4_linux_amd64.tar.gz","browser_download_url":"https://downloads.test/archive"},` +
		`{"name":"checksums.txt","browser_download_url":"https://downloads.test/checksums"}]}`
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case "https://downloads.test/archive":
			return response(http.StatusOK, archive), nil
		case "https://downloads.test/checksums":
			return response(http.StatusOK, []byte(strings.Repeat("0", 64)+"  roast_0.2.4_linux_amd64.tar.gz\n")), nil
		default:
			return response(http.StatusOK, []byte(metadata)), nil
		}
	})}
	destination := filepath.Join(t.TempDir(), "roast")
	if err := os.WriteFile(destination, []byte("trusted old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), "0.2.3", io.Discard, options{
		client:          client,
		executable:      func() (string, error) { return destination, nil },
		operatingSystem: "linux",
		architecture:    "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "ROAST-UPGRADE-CHECKSUM") || !strings.Contains(err.Error(), "binary was not changed") {
		t.Fatalf("error = %v", err)
	}
	content, readErr := os.ReadFile(destination)
	if readErr != nil || string(content) != "trusted old binary" {
		t.Fatalf("content = %q, error = %v", content, readErr)
	}
}

func TestRunNetworkErrorNamesURLAndFallback(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
	err := run(context.Background(), "0.2.3", io.Discard, options{client: client})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, expected := range []string{"ROAST-UPGRADE-RELEASE", latestReleaseURL, "offline", fallbackInstruction()} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("error = %q, missing %q", err, expected)
		}
	}
}

func TestReplaceExecutableKeepsCurrentBinaryWhenSmokeTestFails(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "roast")
	if err := os.WriteFile(destination, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := replaceExecutable(context.Background(), destination, []byte("new"), "0.2.4", func(context.Context, string, string) error {
		return errors.New("wrong architecture")
	})
	if err == nil || !strings.Contains(err.Error(), "ROAST-UPGRADE-SMOKE") {
		t.Fatalf("error = %v", err)
	}
	content, readErr := os.ReadFile(destination)
	if readErr != nil || string(content) != "old" {
		t.Fatalf("content = %q, error = %v", content, readErr)
	}
}

func TestReplaceRunningExecutableOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows replacement contract")
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "roast-helper.exe")
	copyFile(t, source, destination)
	command := exec.Command(destination, "-test.run=TestUpgradeRunningExecutableHelper")
	command.Env = append(os.Environ(), "ROAST_UPGRADE_HELPER=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	if scanner := bufio.NewScanner(stdout); !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("helper did not become ready: %q", scanner.Text())
	}
	replacement := replacementBinary(t, []byte("replacement binary"))
	if err := replaceExecutable(context.Background(), destination, replacement, "0.2.4", func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(content, replacement) {
		t.Fatalf("content = %q, error = %v", content, err)
	}
}

func TestUpgradeRunningExecutableHelper(t *testing.T) {
	if os.Getenv("ROAST_UPGRADE_HELPER") != "1" {
		return
	}
	fmt.Println("ready")
	time.Sleep(time.Minute)
}

func TestExtractZipFindsWindowsBinary(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, err := writer.Create("nested/roast.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("windows binary")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	binary, err := extractBinary("release.zip", "zip", "roast.exe", archive.Bytes())
	if err != nil || string(binary) != "windows binary" {
		t.Fatalf("binary = %q, error = %v", binary, err)
	}
}

func response(status int, content []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(bytes.NewReader(content)),
		Header:     make(http.Header),
	}
}

func tarGzip(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func copyFile(t *testing.T, source, destination string) {
	t.Helper()
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, content, 0o755); err != nil {
		t.Fatal(err)
	}
}

func replacementBinary(t *testing.T, fallback []byte) []byte {
	t.Helper()
	if runtime.GOOS != "windows" {
		return fallback
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func testBinaryName() string {
	if runtime.GOOS == "windows" {
		return "roast.exe"
	}
	return "roast"
}
