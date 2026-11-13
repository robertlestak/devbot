package config

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Service is one service container a repo's runs need, like a CI job's: a
// database the tests talk to, say. Every implement and review run gets its
// own, started for the run and torn down with it.
type Service struct {
	// Name is the container's name, a DNS label.
	Name  string            `yaml:"name" json:"name"`
	Image string            `yaml:"image" json:"image"`
	Env   map[string]string `yaml:"env" json:"env,omitempty"`
	// Command and Args replace the image's entrypoint and command, as a
	// Kubernetes container's do.
	Command []string `yaml:"command" json:"command,omitempty"`
	Args    []string `yaml:"args" json:"args,omitempty"`
	// Ports are the TCP ports the service listens on. The run reaches each
	// on 127.0.0.1, and the run's other services do too.
	Ports     []int            `yaml:"ports" json:"ports,omitempty"`
	Ready     ServiceReady     `yaml:"ready" json:"ready"`
	Resources ServiceResources `yaml:"resources" json:"resources"`
}

// ServiceReady is how a service shows it is ready: a TCP port that accepts
// connections, or a command in its container that exits 0. Neither: ready
// once it is running.
type ServiceReady struct {
	TCP  int      `yaml:"tcp" json:"tcp,omitempty"`
	Exec []string `yaml:"exec" json:"exec,omitempty"`
}

// ServiceResources are a container's CPU and memory, as Kubernetes
// quantities ("500m", "2", "512Mi", "4Gi"). Empty is the install's default.
type ServiceResources struct {
	CPU    string `yaml:"cpu" json:"cpu,omitempty"`
	Memory string `yaml:"memory" json:"memory,omitempty"`
}

// ServiceLimits are the install's rules for the services a repo asks for:
// anyone who can push writes .devbot.yaml, and the install runs the images.
type ServiceLimits struct {
	// Images are the prefixes a service's image must start with, after a
	// short name is expanded the way Docker does ("postgres:17" is
	// "docker.io/library/postgres:17"). Empty allows none; AnyImage, on its
	// own, allows every image.
	Images []string `yaml:"images"`
	// Max is the most services one run may have: 0 forbids them, unset
	// is the default.
	Max *int `yaml:"max"`
	// CPU and Memory are the most one service may ask for (its limit).
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

// Services is how serve runs the service pods repos ask for.
type Services struct {
	// Enabled has serve start each run's service pod. Off, a repo that asks
	// for services is stopped with a message saying so.
	Enabled bool `yaml:"enabled"`
	// Instance is the release whose job pods get services: a lease naming
	// any other pod gets none.
	Instance string `yaml:"instance"`
	// ReadyTimeout is how long a run's services have, from the run asking,
	// to be pulled, started and ready the first time.
	ReadyTimeout time.Duration `yaml:"ready_timeout"`
	// Resources are a service's requests and limits when it doesn't set
	// its own. One that sets a limit below the default request is given
	// that limit as its request too. Limits.EphemeralStorage is every
	// service's, so none can fill its node's disk; a repo can't set it.
	Resources struct {
		Requests ServiceResources `yaml:"requests"`
		Limits   struct {
			ServiceResources `yaml:",inline"`
			EphemeralStorage string `yaml:"ephemeral_storage"`
		} `yaml:"limits"`
	} `yaml:"resources"`
	// ActiveDeadline bounds a service pod's life by its run's role, as
	// jobs.<role>.activeDeadlineSeconds does the job pod's: if serve is
	// gone when the run ends, the pod stops by itself.
	ActiveDeadline struct {
		Implement time.Duration `yaml:"implement"`
		Review    time.Duration `yaml:"review"`
	} `yaml:"active_deadline"`
	// ServiceAccount is the service pods' ServiceAccount, which gets no API
	// token. Empty: the namespace's default.
	ServiceAccount string `yaml:"service_account"`
	// UserNamespaces runs service pods with hostUsers: false.
	UserNamespaces   bool              `yaml:"user_namespaces"`
	RuntimeClassName string            `yaml:"runtime_class_name"`
	NodeSelector     map[string]string `yaml:"node_selector"`
	// Tolerations and Affinity are the pod spec's, as they are.
	Tolerations []map[string]any `yaml:"tolerations"`
	Affinity    map[string]any   `yaml:"affinity"`
	// NetworkPolicy has serve give each service pod a NetworkPolicy that
	// admits its own run's job pod and nothing else.
	NetworkPolicy bool `yaml:"network_policy"`
	// ImagePullSecrets are the Secrets, by name, the kubelet pulls service
	// images with. Only the pod spec names them: no container gets one.
	ImagePullSecrets []string `yaml:"image_pull_secrets"`
}

// AnyImage in repos.limits.services.images allows every image: then
// whoever can push a .devbot.yaml chooses what the install runs.
const AnyImage = "*"

// AllowsImage says whether an image, already normalized, is one the
// install's services may run: one an entry names exactly, or one under an
// entry ending in /, : or @.
func (l ServiceLimits) AllowsImage(img string) bool {
	return slices.Equal(l.Images, []string{AnyImage}) ||
		slices.ContainsFunc(l.Images, func(p string) bool { return img == p || imagePrefix(p) && strings.HasPrefix(img, p) })
}

// imagePrefix is an images entry that ends at a boundary of an image
// reference, so it can't match a longer name: ghcr.io/acme/ is the acme
// namespace, while ghcr.io/acme would also be ghcr.io/acme-evil/x.
func imagePrefix(p string) bool {
	return strings.HasSuffix(p, "/") || strings.HasSuffix(p, ":") || strings.HasSuffix(p, "@")
}

// exactImage is an images entry that names one image, by tag or digest.
func exactImage(p string) bool {
	return strings.Contains(p, "@") || strings.Contains(p[strings.LastIndex(p, "/")+1:], ":")
}

// ModelProxyPort is where the model proxy listens in every job pod, so no
// service may take it.
const ModelProxyPort = 8462

var (
	serviceNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`)
	envNameRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// reservedEnv are names env may not set: the agent sets them for its CLI
// and the model proxy, or they say where the CLI's requests go.
var (
	reservedEnvPrefixes = []string{"DEVBOT_", "ANTHROPIC_", "CLAUDE_", "CODEX_", "OPENAI_", "AWS_", "OTEL_"}
	reservedEnv         = []string{"PATH", "HOME", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}
)

// hookEnv are names env may not set because they have a loader, a shell or
// an interpreter run code or read config from where they point. env comes
// from the base branch, but its value can point into the checkout
// (NODE_OPTIONS=--require ./x.js), and the agent's CLI runs with it: the
// reviewer's would run what the implementer wrote, IgnoreRepoConfig or not.
var (
	hookEnvPrefixes = []string{"LD_", "DYLD_", "GIT_", "XDG_", "NPM_CONFIG_", "npm_config_"}
	hookEnv         = []string{
		"SHELL", "BASH_ENV", "ENV", "ZDOTDIR", "SHELLOPTS", "BASHOPTS", "PS4", "PROMPT_COMMAND", "IFS",
		"GCONV_PATH", "EDITOR", "VISUAL", "PAGER",
		"NODE_OPTIONS", "NODE_PATH",
		"PYTHONSTARTUP", "PYTHONPATH", "PYTHONHOME", "PYTHONUSERBASE",
		"PERL5OPT", "PERL5LIB", "PERLLIB", "PERL5DB",
		"RUBYOPT", "RUBYLIB",
		"JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS",
	}
)

func listed(k string, names, prefixes []string) bool {
	return slices.Contains(names, k) || slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(k, p) })
}

func envList(names, prefixes []string) string {
	return strings.Join(names, ", ") + ", " + strings.Join(prefixes, "*, ") + "*"
}

// ValidateServices checks what a .devbot.yaml's services and env say on
// their own; Config.CheckServices holds them to the install's limits.
func (r *Repo) ValidateServices() error {
	for _, k := range slices.Sorted(maps.Keys(r.Env)) {
		if !envNameRE.MatchString(k) {
			return fmt.Errorf("env: %q isn't a variable name (letters, digits and _, not starting with a digit)", k)
		}
		if listed(k, reservedEnv, reservedEnvPrefixes) {
			return fmt.Errorf("env: %s is devbot's to set (%s)", k, envList(reservedEnv, reservedEnvPrefixes))
		}
		if listed(k, hookEnv, hookEnvPrefixes) {
			return fmt.Errorf("env: %s has a loader, shell or interpreter run code from where it points, which could be the checkout, so a repo can't set it (%s)", k, envList(hookEnv, hookEnvPrefixes))
		}
	}
	names, ports := map[string]bool{}, map[int]string{}
	for i, s := range r.Services {
		at := fmt.Sprintf("services[%d]", i)
		if !serviceNameRE.MatchString(s.Name) {
			return fmt.Errorf("%s.name: %q: want lowercase letters, digits and -, starting with a letter, at most 32", at, s.Name)
		}
		at = "services." + s.Name
		if names[s.Name] {
			return fmt.Errorf("%s: two services have this name", at)
		}
		names[s.Name] = true
		if s.Image == "" || strings.ContainsAny(s.Image, " \t\n") {
			return fmt.Errorf("%s.image: %q isn't an image reference", at, s.Image)
		}
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			if !envNameRE.MatchString(k) {
				return fmt.Errorf("%s.env: %q isn't a variable name", at, k)
			}
		}
		for _, p := range s.Ports {
			switch {
			case p < 1024 || p > 65535:
				// The job pod's harness, unprivileged, listens on each.
				return fmt.Errorf("%s.ports: %d: want 1024 to 65535", at, p)
			case p == ModelProxyPort:
				return fmt.Errorf("%s.ports: %d is the model proxy's, in every job pod", at, p)
			case ports[p] != "":
				return fmt.Errorf("%s.ports: %d is also %s's", at, p, ports[p])
			}
			ports[p] = s.Name
		}
		if s.Ready.TCP != 0 && len(s.Ready.Exec) > 0 {
			return fmt.Errorf("%s.ready: set tcp or exec, not both", at)
		}
		if s.Ready.TCP < 0 || s.Ready.TCP > 65535 {
			return fmt.Errorf("%s.ready.tcp: %d isn't a port", at, s.Ready.TCP)
		}
		if len(s.Ready.Exec) > 0 && s.Ready.Exec[0] == "" {
			return fmt.Errorf("%s.ready.exec: the command is empty", at)
		}
		if _, err := ParseCPU(s.Resources.CPU); s.Resources.CPU != "" && err != nil {
			return fmt.Errorf("%s.resources.cpu: %w", at, err)
		}
		if _, err := ParseMemory(s.Resources.Memory); s.Resources.Memory != "" && err != nil {
			return fmt.Errorf("%s.resources.memory: %w", at, err)
		}
	}
	return nil
}

// ErrNoServices means the install doesn't run service containers at all.
var ErrNoServices = errors.New("this install doesn't run service containers (services.enabled is off)")

// CheckServices holds a repo's services to the install's limits. Unlike the
// other limits, which cap a value quietly, a service outside them is
// refused: an image the install doesn't allow can't be swapped for another.
func (c *Config) CheckServices(r *Repo) error {
	if len(r.Services) == 0 {
		return nil
	}
	if !c.Services.Enabled {
		return ErrNoServices
	}
	l := c.Repos.Limits.Services
	if *l.Max == 0 {
		return errors.New("services: this install doesn't allow a repo any (repos.limits.services.max is 0)")
	}
	if len(r.Services) > *l.Max {
		return fmt.Errorf("services: %d of them, and this install allows at most %d a run (repos.limits.services.max)", len(r.Services), *l.Max)
	}
	maxCPU, _ := ParseCPU(l.CPU)
	maxMem, _ := ParseMemory(l.Memory)
	for _, s := range r.Services {
		img := NormalizeImage(s.Image)
		if !l.AllowsImage(img) {
			allowed := "none"
			if len(l.Images) > 0 {
				allowed = strings.Join(l.Images, ", ")
			}
			return fmt.Errorf("services.%s.image: %s isn't from a registry or repository this install allows (repos.limits.services.images: %s)", s.Name, img, allowed)
		}
		_, lim := c.ServiceResources(s)
		if cpu, _ := ParseCPU(lim.CPU); cpu > maxCPU {
			return fmt.Errorf("services.%s.resources.cpu: %s is over this install's ceiling of %s a service (repos.limits.services.cpu)", s.Name, lim.CPU, l.CPU)
		}
		if mem, _ := ParseMemory(lim.Memory); mem > maxMem {
			return fmt.Errorf("services.%s.resources.memory: %s is over this install's ceiling of %s a service (repos.limits.services.memory)", s.Name, lim.Memory, l.Memory)
		}
	}
	return nil
}

// ServiceResources are what a service's container is given: its own
// resources as its limits, else the install's default, and the default
// requests, brought down to the limits.
func (c *Config) ServiceResources(s Service) (requests, limits ServiceResources) {
	d := c.Services.Resources
	requests, limits = d.Requests, d.Limits.ServiceResources
	if s.Resources.CPU != "" {
		limits.CPU = s.Resources.CPU
	}
	if s.Resources.Memory != "" {
		limits.Memory = s.Resources.Memory
	}
	if r, _ := ParseCPU(requests.CPU); r > mustCPU(limits.CPU) {
		requests.CPU = limits.CPU
	}
	if r, _ := ParseMemory(requests.Memory); r > mustMemory(limits.Memory) {
		requests.Memory = limits.Memory
	}
	return requests, limits
}

func mustCPU(s string) float64    { v, _ := ParseCPU(s); return v }
func mustMemory(s string) float64 { v, _ := ParseMemory(s); return v }

// NormalizeImage expands a short image name the way Docker does, so a
// prefix check sees the registry it is pulled from: "postgres:17" is
// docker.io/library/postgres:17 and "openbao/openbao" is
// docker.io/openbao/openbao.
func NormalizeImage(img string) string {
	first, rest, ok := strings.Cut(img, "/")
	if !ok {
		return "docker.io/library/" + img
	}
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return "docker.io/" + img
	}
	if first == "index.docker.io" {
		return "docker.io/" + rest
	}
	return img
}

var quantityRE = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?)([a-zA-Z]*)$`)

var quantitySuffixes = map[string]float64{
	"": 1, "m": 1e-3, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15,
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50,
}

func parseQuantity(s string) (float64, error) {
	m := quantityRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q isn't a Kubernetes quantity", s)
	}
	f, ok := quantitySuffixes[m[3]]
	if !ok {
		return 0, fmt.Errorf("%q: unknown suffix %q", s, m[3])
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	return v * f, nil
}

// ParseCPU parses a CPU quantity, in cores: "500m" is 0.5.
func ParseCPU(s string) (float64, error) {
	v, err := parseQuantity(s)
	if err == nil && v <= 0 {
		err = fmt.Errorf("%q: want more than none", s)
	}
	return v, err
}

// ParseMemory parses a memory quantity, in bytes: "1Gi" is 1<<30.
func ParseMemory(s string) (float64, error) {
	v, err := parseQuantity(s)
	if err == nil && v < 1<<20 {
		err = fmt.Errorf("%q: want at least 1Mi", s)
	}
	return v, err
}

// servicesDefaults fills the services settings and limits Load leaves zero.
func (c *Config) servicesDefaults() {
	s := &c.Services
	if s.ReadyTimeout == 0 {
		s.ReadyTimeout = 5 * time.Minute
	}
	rs := &s.Resources
	for p, d := range map[*string]string{&rs.Requests.CPU: "100m", &rs.Requests.Memory: "256Mi", &rs.Limits.CPU: "1", &rs.Limits.Memory: "1Gi", &rs.Limits.EphemeralStorage: "2Gi"} {
		if *p == "" {
			*p = d
		}
	}
	if s.ActiveDeadline.Implement == 0 {
		s.ActiveDeadline.Implement = 70 * time.Minute
	}
	if s.ActiveDeadline.Review == 0 {
		s.ActiveDeadline.Review = 30 * time.Minute
	}
	l := &c.Repos.Limits.Services
	if l.Max == nil {
		l.Max = new(4)
	}
	if l.CPU == "" {
		l.CPU = "2"
	}
	if l.Memory == "" {
		l.Memory = "4Gi"
	}
}

func (c *Config) validateServices() error {
	s, l := c.Services, c.Repos.Limits.Services
	if s.Enabled && s.Instance == "" {
		return errors.New("services.enabled needs services.instance: the release whose job pods get services")
	}
	if s.ReadyTimeout < 0 || s.ActiveDeadline.Implement < 0 || s.ActiveDeadline.Review < 0 {
		return errors.New("services: ready_timeout and active_deadline must be positive")
	}
	if slices.Contains(s.ImagePullSecrets, "") {
		return errors.New("services.image_pull_secrets: a Secret's name can't be empty")
	}
	for _, p := range l.Images {
		if p == AnyImage {
			// Beside prefixes it would make them look like they matter.
			if len(l.Images) > 1 {
				return fmt.Errorf("repos.limits.services.images: %q allows every image, so list it alone", AnyImage)
			}
			continue
		}
		// "registry.example.com" alone would also match
		// registry.example.com.evil.net/x, and "ghcr.io/acme"
		// ghcr.io/acme-evil/x.
		if !strings.Contains(p, "/") {
			return fmt.Errorf("repos.limits.services.images: %q: end a registry with /, like %s/", p, p)
		}
		if !imagePrefix(p) && !exactImage(p) {
			return fmt.Errorf("repos.limits.services.images: %q also matches %s-other/...: end a prefix with /, : or @ (like %s/), or name one image with its tag or digest", p, p, p)
		}
	}
	if *l.Max < 0 {
		return errors.New("repos.limits.services.max must be positive")
	}
	maxCPU, err := ParseCPU(l.CPU)
	if err != nil {
		return fmt.Errorf("repos.limits.services.cpu: %w", err)
	}
	maxMem, err := ParseMemory(l.Memory)
	if err != nil {
		return fmt.Errorf("repos.limits.services.memory: %w", err)
	}
	if _, err := ParseMemory(s.Resources.Limits.EphemeralStorage); err != nil {
		return fmt.Errorf("services.resources.limits.ephemeral_storage: %w", err)
	}
	for _, q := range []struct {
		key, v string
		parse  func(string) (float64, error)
		max    float64
	}{
		{"requests.cpu", s.Resources.Requests.CPU, ParseCPU, maxCPU}, {"requests.memory", s.Resources.Requests.Memory, ParseMemory, maxMem},
		{"limits.cpu", s.Resources.Limits.CPU, ParseCPU, maxCPU}, {"limits.memory", s.Resources.Limits.Memory, ParseMemory, maxMem},
	} {
		v, err := q.parse(q.v)
		if err != nil {
			return fmt.Errorf("services.resources.%s: %w", q.key, err)
		}
		// Else a service that asks for nothing would be refused.
		if strings.HasPrefix(q.key, "limits.") && v > q.max {
			return fmt.Errorf("services.resources.%s: %s is over repos.limits.services.%s", q.key, q.v, strings.TrimPrefix(q.key, "limits."))
		}
	}
	return nil
}
