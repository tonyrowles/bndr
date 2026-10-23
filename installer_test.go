package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func releaseFixture(t *testing.T, dir, version string) string {
	t.Helper()
	arch := runtime.GOARCH
	archive := filepath.Join(dir, "bndr_linux_"+arch+".tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	binary := []byte("#!/bin/sh\nprintf 'bndr " + version + "\\n'\n")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "bndr", Mode: 0755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	gzipWriter.Close()
	file.Close()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(data), filepath.Base(archive))), 0600); err != nil {
		t.Fatal(err)
	}
	return archive
}
func installer(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	command := exec.Command("sh", append([]string{"install.sh"}, args...)...)
	command.Env = env
	output, err := command.CombinedOutput()
	return string(output), err
}
func TestInstallerUpgradeChecksumsAndUninstall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux installer")
	}
	root := t.TempDir()
	release := filepath.Join(root, "release")
	os.Mkdir(release, 0700)
	archive := releaseFixture(t, release, "v0.1.0")
	bin := filepath.Join(root, "space in bin")
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "state")
	env := append(os.Environ(), "XDG_CONFIG_HOME="+config, "XDG_STATE_HOME="+state)
	run := func(args ...string) string {
		t.Helper()
		out, err := installer(t, env, args...)
		if err != nil {
			t.Fatalf("installer failed: %v\n%s", err, out)
		}
		return out
	}
	run("--archive", archive, "--bin-dir", bin)
	binary := filepath.Join(bin, "bndr")
	out, err := exec.Command(binary, "--version").Output()
	if err != nil || string(out) != "bndr v0.1.0\n" {
		t.Fatalf("%s %v", out, err)
	}
	os.MkdirAll(filepath.Join(state, "bndr"), 0700)
	saved := filepath.Join(state, "bndr", "apps.json")
	os.WriteFile(saved, []byte("saved settings"), 0600)
	// Failed verification must leave the previous binary intact.
	os.WriteFile(archive, []byte("corrupted archive"), 0600)
	if out, err := installer(t, env, "--archive", archive, "--bin-dir", bin); err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("%v %s", err, out)
	}
	out, _ = exec.Command(binary, "--version").Output()
	if string(out) != "bndr v0.1.0\n" {
		t.Fatal("failed install changed binary")
	}
	archive = releaseFixture(t, release, "v0.2.0")
	run("--archive", archive, "--bin-dir", bin)
	out, _ = exec.Command(binary, "--version").Output()
	if string(out) != "bndr v0.2.0\n" {
		t.Fatal("upgrade failed")
	}
	if _, err := os.Stat(filepath.Join(config, "systemd/user/bndr.service")); !os.IsNotExist(err) {
		t.Fatal("default install created a service")
	}
	run("--uninstall", "--bin-dir", bin)
	if _, err := os.Stat(binary); !os.IsNotExist(err) {
		t.Fatal("uninstall left binary")
	}
	data, _ := os.ReadFile(saved)
	if string(data) != "saved settings" {
		t.Fatal("uninstall changed saved settings")
	}
}
func TestInstallerServiceAndOnlineDownload(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux installer")
	}
	root := t.TempDir()
	release := filepath.Join(root, "release")
	os.Mkdir(release, 0700)
	releaseFixture(t, release, "v0.3.0")
	tools := filepath.Join(root, "tools")
	os.Mkdir(tools, 0700)
	log := filepath.Join(root, "systemctl.log")
	// Mock network and systemctl so tests never change the real user service.
	systemctl := `#!/bin/sh
printf '%s\n' "$*" >> "$BNDR_TEST_LOG"
`
	curl := `#!/bin/sh
output=
url=
while [ "$#" -gt 0 ]; do
 case "$1" in
  -o) output=$2; shift 2;;
  --proto|--proto-redir|--connect-timeout|--max-time|--retry|-w) shift 2;;
  -*) shift;;
  *) url=$1; shift;;
 esac
done
case "$url" in
 https://github.com/tonyrowles/bndr/releases/latest) printf 'https://github.com/tonyrowles/bndr/releases/tag/v0.3.0';;
 https://github.com/tonyrowles/bndr/releases/download/v0.3.0/*) cp "$BNDR_TEST_RELEASE/${url##*/}" "$output";;
 *) echo "Unexpected URL: $url" >&2; exit 1;;
esac
`
	for name, body := range map[string]string{"systemctl": systemctl, "curl": curl} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(root, "config with spaces%")
	bin := filepath.Join(root, "bin with space%$")
	env := append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"), "XDG_CONFIG_HOME="+config, "BNDR_TEST_LOG="+log, "BNDR_TEST_RELEASE="+release)
	out, err := installer(t, env, "--bin-dir", bin, "--service")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	unit := filepath.Join(config, "systemd/user/bndr.service")
	data, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `ExecStart=":`+strings.ReplaceAll(filepath.Join(bin, "bndr"), "%", "%%")+`"`) {
		t.Fatal(string(data))
	}
	// Ask systemd's parser to validate the generated paths and directives.
	if tool, err := exec.LookPath("systemd-analyze"); err == nil {
		command := exec.Command(tool, "--user", "verify", unit)
		command.Env = env
		if output, err := command.CombinedOutput(); err != nil && !strings.Contains(string(output), "Failed to lookup RuntimeDirectory path") {
			t.Fatalf("unit validation: %v\n%s", err, output)
		}
	}
	envFile := filepath.Join(config, "bndr/bndr.env")
	custom := []byte("BNDR_PORT=4010\n")
	os.WriteFile(envFile, custom, 0600)
	out, err = installer(t, env, "--bin-dir", bin, "--service")
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	data, _ = os.ReadFile(envFile)
	if string(data) != string(custom) {
		t.Fatal("upgrade overwrote service configuration")
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "--user restart bndr.service") {
		t.Fatal("service was not restarted")
	}
	out, err = installer(t, env, "--bin-dir", bin, "--uninstall")
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatal("uninstall left service")
	}
	data, _ = os.ReadFile(envFile)
	if string(data) != string(custom) {
		t.Fatal("uninstall changed service configuration")
	}
}
