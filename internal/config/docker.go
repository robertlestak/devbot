package config

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// A repo whose policy leaves docker unset gets a Docker daemon when its base
// branch shows that its tests start containers: a Testcontainers dependency
// or a Compose file, at the root or up to dockerDepth directories down. The
// base branch, not a PR's head, so a task can't turn it on for itself.
const (
	dockerDepth = 2
	// dockerReads caps the manifests read per repo and commit, shallowest
	// first, and dockerReadMax the size of each: a manifest is small, and
	// a big file of the same name is something else.
	dockerReads   = 16
	dockerReadMax = 512 << 10
	// dockerCacheMax caps the commits remembered, all repos together.
	dockerCacheMax = 4096
)

// DockerSignal is what in a repo turned its Docker daemon on: the file, and
// what about it.
type DockerSignal struct {
	Path string
	Why  string
}

// String is the signal for a comment or the model: "`go.mod` depends on
// Testcontainers".
func (s DockerSignal) String() string {
	if s.Path == "" {
		return ""
	}
	return ShowPath(s.Path) + " " + s.Why
}

var composeFiles = []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}

// dockerManifest reports whether a file named name, read as b, depends on
// Testcontainers, and whether name is a manifest to read at all.
func dockerManifest(name string) func([]byte) bool {
	switch {
	case name == "go.mod":
		return goModTestcontainers
	case name == "package.json":
		return packageJSONTestcontainers
	case name == "pyproject.toml", name == "setup.cfg", name == "Pipfile",
		strings.HasPrefix(name, "requirements") && strings.HasSuffix(name, ".txt"):
		return matches(pythonTC)
	case name == "pom.xml", name == "build.gradle", name == "build.gradle.kts":
		return matches(jvmTC)
	case strings.HasSuffix(name, ".csproj"), name == "Directory.Packages.props":
		return matches(dotnetTC)
	case name == "Cargo.toml":
		return matches(rustTC)
	}
	return nil
}

var (
	// PyPI names are case-insensitive, and an extra or a version may follow.
	pythonTC = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.-])testcontainers($|[^A-Za-z0-9_-])`)
	jvmTC    = regexp.MustCompile(`\borg\.testcontainers\b`)
	dotnetTC = regexp.MustCompile(`\bTestcontainers\b`)
	rustTC   = regexp.MustCompile(`(^|[^A-Za-z0-9_-])testcontainers($|[^A-Za-z0-9_])`)
)

func matches(re *regexp.Regexp) func([]byte) bool { return re.Match }

const goTC = "github.com/testcontainers/testcontainers-go"

// goModTestcontainers: a require of the module or any under it, on a line
// of its own or in a require block.
func goModTestcontainers(b []byte) bool {
	block := false
	for line := range strings.Lines(string(b)) {
		line, _, _ = strings.Cut(line, "//")
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case block && f[0] == ")":
			block = false
		case block:
			if f[0] == goTC || strings.HasPrefix(f[0], goTC+"/") {
				return true
			}
		case f[0] == "require" && len(f) > 1 && f[1] == "(":
			block = true
		case f[0] == "require" && len(f) > 1:
			if f[1] == goTC || strings.HasPrefix(f[1], goTC+"/") {
				return true
			}
		}
	}
	return false
}

// packageJSONTestcontainers: testcontainers or an @testcontainers/ package
// in any of the dependency sections.
func packageJSONTestcontainers(b []byte) bool {
	var p map[string]json.RawMessage
	if json.Unmarshal(b, &p) != nil {
		return false
	}
	for _, sec := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"} {
		var deps map[string]any
		if json.Unmarshal(p[sec], &deps) != nil {
			continue
		}
		for name := range deps {
			if name == "testcontainers" || strings.HasPrefix(name, "@testcontainers/") {
				return true
			}
		}
	}
	return false
}

// DetectDocker looks on repo's branch for a sign that its tests start
// containers: one BranchHead, then per commit one ListTree and at most
// dockerReads small reads, remembered by commit. A zero signal is none. An
// error, the tree's included, means devbot can't tell.
func (c *RepoCache) DetectDocker(ctx context.Context, repo forge.RepoRef, branch string) (DockerSignal, error) {
	p, err := c.Forges.For(repo)
	if err != nil {
		return DockerSignal{}, err
	}
	sha, err := p.BranchHead(ctx, repo, branch)
	if err != nil {
		return DockerSignal{}, fmt.Errorf("reading %s's head: %w", branch, err)
	}
	key := repo.String() + "@" + sha
	c.mu.Lock()
	s, ok := c.signals[key]
	c.mu.Unlock()
	if ok {
		return s, nil
	}
	s, err = detectDocker(ctx, p, repo, sha)
	if err != nil {
		return DockerSignal{}, err
	}
	c.mu.Lock()
	if c.signals == nil {
		c.signals = map[string]DockerSignal{}
	}
	// One arbitrary entry makes room rather than all of them, so a pass
	// over more repos than fit still finds most remembered.
	for k := range c.signals {
		if len(c.signals) < dockerCacheMax {
			break
		}
		delete(c.signals, k)
	}
	c.signals[key] = s
	c.mu.Unlock()
	return s, nil
}

func detectDocker(ctx context.Context, p forge.Provider, repo forge.RepoRef, sha string) (DockerSignal, error) {
	files, err := p.ListTree(ctx, repo, sha)
	if err != nil {
		return DockerSignal{}, fmt.Errorf("listing the files at %s: %w", sha, err)
	}
	depth := func(f string) int { return strings.Count(f, "/") }
	var near []forge.TreeFile
	for _, f := range files {
		if depth(f.Path) <= dockerDepth {
			near = append(near, f)
		}
	}
	slices.SortFunc(near, func(a, b forge.TreeFile) int {
		if d := depth(a.Path) - depth(b.Path); d != 0 {
			return d
		}
		return strings.Compare(a.Path, b.Path)
	})
	// A Compose file says so by its name alone.
	for _, f := range near {
		if slices.Contains(composeFiles, path.Base(f.Path)) {
			return DockerSignal{Path: f.Path, Why: "is a Docker Compose file"}, nil
		}
	}
	reads := 0
	for _, f := range near {
		match := dockerManifest(path.Base(f.Path))
		if match == nil || f.Size > dockerReadMax {
			continue
		}
		if reads == dockerReads {
			break
		}
		reads++
		b, err := p.GetFile(ctx, repo, sha, f.Path)
		if err != nil {
			return DockerSignal{}, fmt.Errorf("reading %s at %s: %w", f.Path, sha, err)
		}
		if match(b) {
			return DockerSignal{Path: f.Path, Why: "depends on Testcontainers"}, nil
		}
	}
	return DockerSignal{}, nil
}
