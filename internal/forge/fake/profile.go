package fake

import (
	"os"
	"slices"
)

// Profile is how one forge names what the core and the harness compare:
// the provider name in refs, labels and audit, the two bots' logins, and
// the branches under devbot/ the adapter itself keeps in every repo.
type Profile struct {
	Name      string // the subtest name
	Provider  string
	Impl, Rev string
	// Branches are made in every repo AddRepo adds, besides its default one.
	Branches []string
}

var (
	// Default is the fake's own: plain user logins, as on Gitea.
	Default = Profile{Name: "default", Provider: "fake", Impl: "devbot", Rev: "devbot-review"}
	// GitHub names its bots as Apps, "slug[bot]", and keeps PR attachments
	// on a devbot/attachments branch that belongs to no task.
	GitHub = Profile{Name: "github", Provider: "github", Impl: "acme-devbot[bot]", Rev: "acme-devbot-review[bot]",
		Branches: []string{"devbot/attachments"}}

	Profiles = []Profile{Default, GitHub}
)

// ProfileEnv picks the profile ProfileFromEnv returns, by name, so that a
// whole suite can be run as one forge: DEVBOT_TEST_FORGE=github go test ./...
const ProfileEnv = "DEVBOT_TEST_FORGE"

// ProfileFromEnv is the profile ProfileEnv names, or Default. A name it
// doesn't know panics, so a typo can't pass as a GitHub-shaped run.
func ProfileFromEnv() Profile {
	name := os.Getenv(ProfileEnv)
	if name == "" {
		return Default
	}
	i := slices.IndexFunc(Profiles, func(p Profile) bool { return p.Name == name })
	if i < 0 {
		panic("fake: " + ProfileEnv + "=" + name + " names no profile")
	}
	return Profiles[i]
}

// NewAs is a fake forge named as p names things.
func NewAs(p Profile) *Forge {
	f := New()
	f.ProviderName, f.Impl, f.Rev = p.Provider, p.Impl, p.Rev
	f.Writers = map[string]bool{"alice": true, p.Impl: true, p.Rev: true}
	f.seed = slices.Clone(p.Branches)
	return f
}
