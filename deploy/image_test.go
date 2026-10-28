package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s isn't on the PATH", tool)
		}
	}
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// script runs one of the repo's scripts with env added, and returns its
// combined output.
func script(t *testing.T, env []string, path string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(path, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// A download is kept only with the SHA-256 it was pinned to: a tampered
// pin, or a tampered file, fails the build and leaves nothing behind.
func TestImageFetchVerifies(t *testing.T) {
	needTools(t, "sh", "curl", "sha256sum")
	body := []byte("a release binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "bin")

	if msg, err := script(t, nil, "../docker/fetch", srv.URL+"/bin", sum(body), out); err != nil {
		t.Fatalf("the right SHA-256: %v\n%s", err, msg)
	}
	if b, _ := os.ReadFile(out); string(b) != string(body) {
		t.Errorf("fetched %q", b)
	}
	os.Remove(out)

	tampered := sum(append(body, '!'))
	for name, pin := range map[string]string{"tampered": tampered, "empty": "", "not hex": strings.Repeat("z", 64), "short": tampered[:63]} {
		msg, err := script(t, nil, "../docker/fetch", srv.URL+"/bin", pin, out)
		if err == nil {
			t.Errorf("%s pin: fetch succeeded", name)
		}
		if name == "tampered" && !strings.Contains(msg, "want "+tampered) {
			t.Errorf("tampered pin: %s", msg)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s pin: %s was left behind", name, out)
			os.Remove(out)
		}
	}
}

// Claude Code is checked against the SHA-256 in Anthropic's manifest for
// the version, for the build's own architecture.
func TestImageFetchClaude(t *testing.T) {
	needTools(t, "sh", "curl", "sha256sum", "jq")
	x64, arm := []byte("claude linux-x64"), []byte("claude linux-arm64")
	manifest := fmt.Sprintf(`{"version": "2.1.299", "platforms": {"linux-x64": {"checksum": %q}, "linux-arm64": {"checksum": %q}}}`, sum(x64), sum(arm))
	files := map[string]string{
		"/2.1.299/manifest.json":      manifest,
		"/2.1.299/linux-x64/claude":   string(x64),
		"/2.1.299/linux-arm64/claude": string(arm),
		// A manifest whose checksum doesn't match its binary.
		"/2.1.300/manifest.json":    fmt.Sprintf(`{"version": "2.1.300", "platforms": {"linux-x64": {"checksum": %q}}}`, sum(arm)),
		"/2.1.300/linux-x64/claude": string(x64),
		// One served for the wrong version.
		"/2.1.301/manifest.json": manifest,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(b))
	}))
	defer srv.Close()
	env := []string{"CLAUDE_DOWNLOAD_URL=" + srv.URL}

	for arch, want := range map[string][]byte{"amd64": x64, "arm64": arm} {
		out := filepath.Join(t.TempDir(), "claude")
		if msg, err := script(t, env, "../docker/fetch-claude", "2.1.299", arch, out); err != nil {
			t.Fatalf("%s: %v\n%s", arch, err, msg)
		}
		if b, _ := os.ReadFile(out); string(b) != string(want) {
			t.Errorf("%s: fetched %q", arch, b)
		}
	}
	for _, c := range []struct{ version, arch, msg string }{
		{"2.1.300", "amd64", "SHA-256 is"},
		{"2.1.300", "arm64", "no linux-arm64 build"},
		{"2.1.301", "amd64", "is for '2.1.299'"},
		{"2.1.302", "amd64", ""},
		{"2.1.299", "riscv64", "unsupported architecture"},
	} {
		out := filepath.Join(t.TempDir(), "claude")
		msg, err := script(t, env, "../docker/fetch-claude", c.version, c.arch, out)
		if err == nil || !strings.Contains(msg, c.msg) {
			t.Errorf("%s %s: %v, %s", c.version, c.arch, err, msg)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s %s: %s was left behind", c.version, c.arch, out)
		}
	}
}

// hack/image-versions reports what's behind and, with -w, rewrites the
// pins in both Dockerfiles with the new releases' published SHA-256s.
func TestImageVersions(t *testing.T) {
	needTools(t, "sh", "curl", "sed", "awk", "grep")
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "docker"), 0o755)
	for _, f := range []string{"Dockerfile", "claude.Dockerfile"} {
		b, err := os.ReadFile(filepath.Join("../docker", f))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(root, "docker", f), b, 0o644)
	}
	pinned := func(arg string) string {
		b, _ := os.ReadFile(filepath.Join(root, "docker/Dockerfile"))
		m := regexp.MustCompile(`(?m)^ARG ` + arg + `=(.*)$`).FindSubmatch(b)
		if m == nil {
			t.Fatalf("docker/Dockerfile has no ARG %s", arg)
		}
		return string(m[1])
	}
	claude, codex, mise := pinned("CLAUDE_VERSION"), pinned("CODEX_VERSION"), pinned("MISE_VERSION")
	h := func(s string) string { return sum([]byte(s)) }

	latest := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; p {
		case "/claude/latest":
			fmt.Fprint(w, latest["claude"])
		case "/claude/9.9.9/manifest.json":
			fmt.Fprint(w, `{"version": "9.9.9", "platforms": {"linux-x64": {}, "linux-arm64": {}}}`)
		case "/openai/codex/releases/latest":
			http.Redirect(w, r, "/openai/codex/releases/tag/rust-v"+latest["codex"], http.StatusFound)
		case "/jdx/mise/releases/latest":
			http.Redirect(w, r, "/jdx/mise/releases/tag/"+latest["mise"], http.StatusFound)
		case "/openai/codex/releases/download/rust-v8.8.8/codex-package_SHA256SUMS":
			fmt.Fprintf(w, "%s  codex-package-aarch64-unknown-linux-musl.tar.gz\n%s  codex-package-x86_64-unknown-linux-musl-bazel.tar.gz\n%s  codex-package-x86_64-unknown-linux-musl.tar.gz\n",
				h("codex arm64"), h("bazel"), h("codex amd64"))
		case "/jdx/mise/releases/download/v7.7.7/SHASUMS256.txt":
			fmt.Fprintf(w, "%s  ./mise-v7.7.7-linux-arm64\n%s  ./mise-v7.7.7-linux-x64-musl\n%s  ./mise-v7.7.7-linux-x64\n",
				h("mise arm64"), h("musl"), h("mise amd64"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	env := []string{"IMAGE_VERSIONS_ROOT=" + root, "CLAUDE_DOWNLOAD_URL=" + srv.URL + "/claude", "GITHUB_URL=" + srv.URL}

	latest["claude"], latest["codex"], latest["mise"] = claude, codex, mise
	if out, err := script(t, env, "../hack/image-versions"); err != nil || out != "up to date\n" {
		t.Fatalf("pins current: %v, %q", err, out)
	}

	latest["claude"], latest["codex"], latest["mise"] = "9.9.9", "8.8.8", "v7.7.7"
	want := fmt.Sprintf("claude %s 9.9.9\ncodex %s 8.8.8\nmise %s v7.7.7\n", claude, codex, mise)
	if out, err := script(t, env, "../hack/image-versions"); err != nil || out != want {
		t.Fatalf("behind: %v, %q", err, out)
	}
	if pinned("CLAUDE_VERSION") != claude {
		t.Fatal("without -w the pins moved")
	}
	if out, err := script(t, env, "../hack/image-versions", "-w"); err != nil || out != want {
		t.Fatalf("-w: %v, %q", err, out)
	}
	for arg, v := range map[string]string{
		"CLAUDE_VERSION": "9.9.9",
		"CODEX_VERSION":  "8.8.8", "CODEX_SHA256_AMD64": h("codex amd64"), "CODEX_SHA256_ARM64": h("codex arm64"),
		"MISE_VERSION": "v7.7.7", "MISE_SHA256_AMD64": h("mise amd64"), "MISE_SHA256_ARM64": h("mise arm64"),
	} {
		if got := pinned(arg); got != v {
			t.Errorf("%s = %s, want %s", arg, got, v)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "docker/claude.Dockerfile")); !strings.Contains(string(b), "\nARG CLAUDE_VERSION=9.9.9\n") {
		t.Error("docker/claude.Dockerfile's CLAUDE_VERSION didn't move")
	}
	if out, err := script(t, env, "../hack/image-versions"); err != nil || out != "up to date\n" {
		t.Errorf("after -w: %v, %q", err, out)
	}
}

// The images are reproducible and the base carries no agent CLI: every
// FROM is a digest-pinned image or an earlier stage, and only the codex
// and claude targets put a CLI in.
func TestDockerfileStages(t *testing.T) {
	b, err := os.ReadFile("../docker/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^ARG (\w+)=(.*)$`).FindAllStringSubmatch(string(b), -1) {
		args[m[1]] = m[2]
	}
	stages := map[string]string{}
	var order []string
	var cur string
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 4 && f[0] == "FROM" && f[2] == "AS" {
			from := f[1]
			if strings.HasPrefix(from, "${") {
				from = args[strings.Trim(from, "${}")]
				if !regexp.MustCompile(`^[\w./:-]+@sha256:[0-9a-f]{64}$`).MatchString(from) {
					t.Errorf("stage %s: %q isn't pinned by digest", f[3], from)
				}
			} else if _, ok := stages[from]; !ok {
				t.Errorf("stage %s: FROM %s, not an earlier stage", f[3], from)
			}
			cur = f[3]
			order = append(order, cur)
			continue
		}
		if cur != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			stages[cur] += line + "\n"
		}
	}
	if len(order) == 0 || order[len(order)-1] != "base" {
		t.Errorf("the last stage, the default target, is %v", order)
	}
	for _, target := range []string{"base", "codex", "claude"} {
		if _, ok := stages[target]; !ok {
			t.Errorf("no %s target", target)
		}
	}
	for _, s := range []string{"runtime", "base"} {
		if strings.Contains(stages[s], "claude") || strings.Contains(stages[s], "codex") {
			t.Errorf("stage %s puts an agent CLI in:\n%s", s, stages[s])
		}
	}
	if !strings.Contains(stages["runtime"], "org.opencontainers.image.source=") {
		t.Error("the images have no OCI source label")
	}
	if !strings.Contains(stages["claude"], "Commercial Terms") {
		t.Error("the claude image doesn't say Claude Code is under Anthropic's terms")
	}
}
