package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// servicesYAML is the services a repo like acme/app declares: an OpenBao dev
// server and the Postgres that uses it, both on ports of their own.
const servicesYAML = `
version: 1
services:
  - name: openbao
    image: openbao/openbao:2.4.1
    env: {BAO_DEV_ROOT_TOKEN_ID: root, BAO_DEV_LISTEN_ADDRESS: 0.0.0.0:18237}
    ports: [18237]
    ready: {tcp: 18237}
  - name: pg
    image: registry.example.com/ci-postgres:17
    env: {POSTGRES_PASSWORD: ci, PGPORT: 54337, BAO_ADDR: http://127.0.0.1:18237, BAO_TOKEN: root}
    ports: [54337]
    ready: {exec: [pg_isready, -U, postgres, -h, 127.0.0.1, -p, "54337"]}
    resources: {cpu: 2, memory: 2Gi}
env:
  APP_TEST_DB_DSN: postgres://postgres:ci@localhost:54337/postgres?sslmode=disable
  APP_TEST_KMS_ADDR: http://localhost:18237
`

func servicesConfig(t *testing.T, yaml string) *Config {
	t.Helper()
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte(yaml), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseServices(t *testing.T) {
	r, err := ParseRepo([]byte(servicesYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Services) != 2 || r.Services[1].Name != "pg" || r.Services[1].Env["PGPORT"] != "54337" || r.Services[1].Resources.CPU != "2" {
		t.Fatalf("services: %+v", r.Services)
	}
	if r.Services[0].Ready.TCP != 18237 || len(r.Services[1].Ready.Exec) != 7 {
		t.Fatalf("readiness: %+v", r.Services)
	}
	if !strings.HasPrefix(r.Env["APP_TEST_DB_DSN"], "postgres://") {
		t.Fatalf("env: %v", r.Env)
	}

	for _, c := range []struct{ yaml, want string }{
		{"services: [{name: PG, image: postgres}]", "services[0].name"},
		{"services: [{name: pg}]", "services.pg.image"},
		{"services: [{name: pg, image: postgres}, {name: pg, image: redis}]", "two services"},
		{"services: [{name: pg, image: postgres, ports: [5432]}, {name: pg2, image: postgres, ports: [5432]}]", "also pg's"},
		{"services: [{name: pg, image: postgres, ports: [80]}]", "1024 to 65535"},
		{"services: [{name: pg, image: postgres, ports: [8462]}]", "model proxy"},
		{"services: [{name: pg, image: postgres, ready: {tcp: 5432, exec: [true]}}]", "not both"},
		{"services: [{name: pg, image: postgres, resources: {memory: lots}}]", "resources.memory"},
		{"env: {PATH: /evil}", "devbot's to set"},
		{"env: {ANTHROPIC_BASE_URL: https://evil}", "devbot's to set"},
		{"env: {1X: y}", "isn't a variable name"},
		// A loader or interpreter hook could point into the checkout.
		{"env: {NODE_OPTIONS: --require ./x.js}", "loader, shell or interpreter"},
	} {
		if _, err := ParseRepo([]byte(c.yaml)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error about %q", c.yaml, err, c.want)
		}
	}
}

func TestNormalizeImage(t *testing.T) {
	for in, want := range map[string]string{
		"postgres:17":                         "docker.io/library/postgres:17",
		"openbao/openbao:2.4.1":               "docker.io/openbao/openbao:2.4.1",
		"registry.example.com/ci-postgres:17": "registry.example.com/ci-postgres:17",
		"localhost/x":                         "localhost/x",
		"reg:5000/x":                          "reg:5000/x",
		"index.docker.io/library/postgres:17": "docker.io/library/postgres:17",
	} {
		if got := NormalizeImage(in); got != want {
			t.Errorf("NormalizeImage(%q) = %q, want %q", in, got, want)
		}
	}
}

// A .devbot.yaml can't pull an image the install doesn't allow, run more
// services than it allows or ask for more than its ceilings: it is refused,
// with the key and the limit it broke.
func TestServiceLimits(t *testing.T) {
	r, err := ParseRepo([]byte(servicesYAML))
	if err != nil {
		t.Fatal(err)
	}
	off := servicesConfig(t, "")
	if err := off.CheckServices(r); err != ErrNoServices {
		t.Fatalf("services off: %v", err)
	}
	if err := off.CheckServices(&Repo{}); err != nil {
		t.Fatalf("a repo without services needs nothing: %v", err)
	}

	c := servicesConfig(t, `
services: {enabled: true, instance: devbot}
repos:
  limits:
    services: {images: [registry.example.com/, docker.io/openbao/], cpu: "2", memory: 2Gi}
`)
	if err := c.CheckServices(r); err != nil {
		t.Fatalf("within the limits: %v", err)
	}
	if c.Services.ReadyTimeout != 5*time.Minute || *c.Repos.Limits.Services.Max != 4 || c.Services.Resources.Limits.EphemeralStorage != "2Gi" {
		t.Fatalf("defaults: %+v %+v", c.Services, c.Repos.Limits.Services)
	}
	req, lim := c.ServiceResources(r.Services[0])
	if req.CPU != "100m" || req.Memory != "256Mi" || lim.CPU != "1" || lim.Memory != "1Gi" {
		t.Fatalf("a service without resources gets the defaults: %+v %+v", req, lim)
	}
	small := Service{Resources: ServiceResources{CPU: "50m", Memory: "128Mi"}}
	if req, lim := c.ServiceResources(small); req.CPU != "50m" || req.Memory != "128Mi" || lim.CPU != "50m" {
		t.Fatalf("a request never exceeds its limit: %+v %+v", req, lim)
	}

	for _, tc := range []struct {
		edit func(*Repo)
		want string
	}{
		{func(r *Repo) { r.Services[1].Image = "docker.io/library/postgres:17" }, "repos.limits.services.images"},
		// A short name is Docker Hub's, whatever it looks like.
		{func(r *Repo) { r.Services[1].Image = "registry.example.com" }, "docker.io/library/registry.example.com"},
		{func(r *Repo) { r.Services[1].Resources.CPU = "2500m" }, "repos.limits.services.cpu"},
		{func(r *Repo) { r.Services[1].Resources.Memory = "3Gi" }, "repos.limits.services.memory"},
		{func(r *Repo) {
			for range 3 {
				r.Services = append(r.Services, r.Services[0])
			}
		}, "at most 4"},
	} {
		r, _ := ParseRepo([]byte(servicesYAML))
		tc.edit(r)
		if err := c.CheckServices(r); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v, want an error naming %s", err, tc.want)
		}
	}

	for _, bad := range []string{
		"services: {enabled: true}",
		"repos: {limits: {services: {images: [registry.example.com]}}}",
		"repos: {limits: {services: {memory: 512Mi}}}", // under the default limit
		"services: {resources: {limits: {cpu: lots}}}",
		"services: {resources: {limits: {ephemeral_storage: lots}}}",
		`services: {image_pull_secrets: [regcred, ""]}`,
		// The install's own defaults are held to its limits too.
		"services: {enabled: true, instance: devbot}\nrepos: {defaults: {services: [{name: pg, image: postgres}]}}",
	} {
		p := t.TempDir() + "/config.yaml"
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: loaded", bad)
		}
	}
}

func TestQuantities(t *testing.T) {
	for in, want := range map[string]float64{"500m": 0.5, "2": 2, "1.5": 1.5} {
		if got, err := ParseCPU(in); err != nil || got != want {
			t.Errorf("ParseCPU(%q) = %v, %v", in, got, err)
		}
	}
	for in, want := range map[string]float64{"512Mi": 512 << 20, "1Gi": 1 << 30, "2G": 2e9} {
		if got, err := ParseMemory(in); err != nil || got != want {
			t.Errorf("ParseMemory(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "lots", "1Xi", "-1", "0"} {
		if _, err := ParseCPU(bad); err == nil {
			t.Errorf("ParseCPU(%q) passed", bad)
		}
	}
}

// Names that have a loader, a shell or an interpreter load code from where
// they point are refused, and their relatives with them; names like them
// that don't are not.
func TestHookEnvRefused(t *testing.T) {
	for _, k := range []string{
		"NODE_OPTIONS", "NODE_PATH", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "DYLD_INSERT_LIBRARIES", "SHELL", "BASH_ENV", "ENV",
		"PYTHONSTARTUP", "PYTHONPATH", "PYTHONHOME", "PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB",
		"GIT_CONFIG_GLOBAL", "GIT_SSH_COMMAND", "GIT_EXEC_PATH", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
		"JAVA_TOOL_OPTIONS", "npm_config_node_options", "GCONV_PATH", "EDITOR",
	} {
		_, err := ParseRepo([]byte("env: {" + k + ": ./x}"))
		if err == nil || !strings.Contains(err.Error(), "env: "+k+" has a loader, shell or interpreter run code") {
			t.Errorf("%s: %v", k, err)
		}
	}
	for _, k := range []string{"PYTHONUNBUFFERED", "NODE_ENV", "RUBY_VERSION", "GITHUB_SHA", "DATABASE_URL", "ENVIRONMENT"} {
		if _, err := ParseRepo([]byte("env: {" + k + ": x}")); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
}

// repos.limits.services.max: 0 forbids services; unset is the default.
func TestServicesMaxZero(t *testing.T) {
	r, err := ParseRepo([]byte(servicesYAML))
	if err != nil {
		t.Fatal(err)
	}
	c := servicesConfig(t, "services: {enabled: true, instance: devbot}\nrepos: {limits: {services: {max: 0, images: [registry.example.com/, docker.io/openbao/]}}}")
	if err := c.CheckServices(r); err == nil || !strings.Contains(err.Error(), "repos.limits.services.max is 0") {
		t.Fatalf("max 0: %v", err)
	}
	if err := c.CheckServices(&Repo{}); err != nil {
		t.Fatalf("a repo without services needs nothing: %v", err)
	}
	c = servicesConfig(t, "services: {enabled: true, instance: devbot}\nrepos: {limits: {services: {max: 1, images: [registry.example.com/, docker.io/openbao/]}}}")
	if err := c.CheckServices(r); err == nil || !strings.Contains(err.Error(), "at most 1") {
		t.Fatalf("max 1: %v", err)
	}
}

// images: ["*"] allows every image, and the other limits still hold; "*"
// beside prefixes is refused.
func TestServicesAnyImage(t *testing.T) {
	c := servicesConfig(t, `services: {enabled: true, instance: devbot}
repos: {limits: {services: {images: ["*"], max: 2, cpu: "2", memory: 2Gi}}}`)
	for _, img := range []string{"postgres:17", "docker.io/someone/db:1", "quay.io/x/y@sha256:abc", "localhost:5000/z"} {
		r, _ := ParseRepo([]byte(servicesYAML))
		r.Services[1].Image = img
		if err := c.CheckServices(r); err != nil {
			t.Errorf("%s: %v", img, err)
		}
	}
	for _, tc := range []struct {
		edit func(*Repo)
		want string
	}{
		{func(r *Repo) { r.Services[1].Resources.CPU = "2500m" }, "repos.limits.services.cpu"},
		{func(r *Repo) { r.Services[1].Resources.Memory = "3Gi" }, "repos.limits.services.memory"},
		{func(r *Repo) { r.Services = append(r.Services, r.Services[0]) }, "at most 2"},
	} {
		r, _ := ParseRepo([]byte(servicesYAML))
		tc.edit(r)
		if err := c.CheckServices(r); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v, want an error naming %s", err, tc.want)
		}
	}
	c = servicesConfig(t, "services: {enabled: true, instance: devbot}\nrepos: {limits: {services: {images: [\"*\"], max: 0}}}")
	if r, _ := ParseRepo([]byte(servicesYAML)); c.CheckServices(r) == nil {
		t.Error("max 0 with any image: allowed")
	}

	// "*" is only special alone: as a prefix nothing starts with it.
	if (ServiceLimits{Images: []string{"registry.example.com/"}}).AllowsImage("docker.io/library/postgres:17") ||
		(ServiceLimits{}).AllowsImage("docker.io/library/postgres:17") {
		t.Error("a prefix list or none allows another image")
	}
	// A prefix stops at a boundary; anything else names one image.
	l := ServiceLimits{Images: []string{"ghcr.io/acme/", "docker.io/library/postgres:", "docker.io/library/redis:7", "quay.io/x/y@"}}
	for img, want := range map[string]bool{
		"ghcr.io/acme/db:1":                             true,
		"ghcr.io/acme-evil/x:1":                         false,
		"docker.io/library/postgres:17":                 true,
		"docker.io/library/postgresx:17":                false,
		"docker.io/library/redis:7":                     true,
		"docker.io/library/redis:7-evil":                false,
		"quay.io/x/y@sha256:" + strings.Repeat("a", 64): true,
	} {
		if l.AllowsImage(img) != want {
			t.Errorf("AllowsImage(%s) = %v", img, !want)
		}
	}
	for _, good := range []string{"ghcr.io/acme/", "ghcr.io/acme/db:", "ghcr.io/acme/db@", "ghcr.io/acme/db:16", "ghcr.io/acme/db@sha256:abc", "registry.example.com:5000/"} {
		p := t.TempDir() + "/config.yaml"
		os.WriteFile(p, []byte("repos: {limits: {services: {images: ["+good+"]}}}"), 0o644)
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{"ghcr.io/acme", "registry.example.com:5000/acme"} {
		p := t.TempDir() + "/config.yaml"
		os.WriteFile(p, []byte("repos: {limits: {services: {images: ["+bad+"]}}}"), 0o644)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "end a prefix with /, : or @ (like "+bad+"/)") {
			t.Errorf("%s: %v", bad, err)
		}
	}

	for _, bad := range []string{`["*", registry.example.com/]`, `[registry.example.com/, "*"]`, `["*", "*"]`} {
		p := t.TempDir() + "/config.yaml"
		os.WriteFile(p, []byte("repos: {limits: {services: {images: "+bad+"}}}"), 0o644)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "list it alone") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}
