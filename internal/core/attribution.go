package core

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// Attribution is what a job's runtime and cost are reported by: on its pod's
// labels, its metrics, its log records and the usage line it posts.
type Attribution struct {
	Repo forge.RepoRef
	Task int
	Role string // implement | review
	// Requester is who asked for the task: the principal that filed it
	// through a connector, else the handle it named, else the issue's author.
	Requester string
	Origin    string
	PR        int
	// Labels are the repo's static labels (.devbot.yaml labels), already
	// valid label values.
	Labels map[string]string
}

// NewAttribution attributes a role's job on it, whose routing block is m.
func NewAttribution(it forge.Item, m Meta, role string, pr int, labels map[string]string) Attribution {
	return Attribution{Repo: it.Ref.Repo, Task: it.Ref.Number, Role: role, PR: pr, Origin: m.Origin, Labels: labels,
		Requester: cmp.Or(m.Requester, m.RequesterHandle, it.Author)}
}

// Pod label and annotation keys. Static labels go under their own prefix, so
// a repo's keys can't collide with devbot's.
const (
	PodLabelPrefix       = "devbot.io/"
	PodStaticLabelPrefix = "labels.devbot.io/"
)

// PodLabels are the job pod's labels: each value made a valid label value.
func (a Attribution) PodLabels() map[string]string {
	l := map[string]string{}
	for k, v := range a.podFields() {
		l[PodLabelPrefix+k] = LabelValue(v)
	}
	for k, v := range a.Labels {
		l[PodStaticLabelPrefix+k] = v
	}
	return l
}

// PodAnnotations are the same values unsanitised, with the origin and PR.
func (a Attribution) PodAnnotations() map[string]string {
	an := map[string]string{}
	for k, v := range a.podFields() {
		an[PodLabelPrefix+k] = v
	}
	if a.Origin != "" {
		an[PodLabelPrefix+"origin"] = a.Origin
	}
	if a.PR > 0 {
		an[PodLabelPrefix+"pr"] = strconv.Itoa(a.PR)
	}
	return an
}

func (a Attribution) podFields() map[string]string {
	return map[string]string{"provider": a.Repo.Provider, "owner": a.Repo.Owner, "repo": a.Repo.Name,
		"task": strconv.Itoa(a.Task), "role": a.Role, "requester": a.Requester}
}

// MetricValues are a's values for the attribution label names
// (config.Metrics.AttributionLabels), in their order. Counters are kept in
// Redis under "|"-joined labels, so a value can't carry one.
func (a Attribution) MetricValues(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		var v string
		switch n {
		case "repo":
			v = a.Repo.String()
		case "owner":
			v = a.Repo.Owner
		case "requester":
			v = a.Requester
		default:
			v = a.Labels[strings.TrimPrefix(n, "label_")]
		}
		out[i] = strings.ReplaceAll(v, "|", "_")
	}
	return out
}

// LogAttrs are a's fields for a job's log records.
func (a Attribution) LogAttrs() []any {
	attrs := []any{"repo", a.Repo.String(), "role", a.Role, "requester", a.Requester}
	if len(a.Labels) > 0 {
		attrs = append(attrs, "labels", a.Labels)
	}
	return attrs
}

// String is a for people: "implement of gitea:acme/app#7, requested by
// alice, team=payments".
func (a Attribution) String() string {
	s := fmt.Sprintf("%s of %s#%d", a.Role, a.Repo, a.Task)
	if a.Requester != "" {
		s += ", requested by " + a.Requester
	}
	for _, k := range slices.Sorted(maps.Keys(a.Labels)) {
		s += ", " + k + "=" + a.Labels[k]
	}
	return s
}

var labelBadRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// LabelValue makes s a valid Kubernetes label value: invalid runs become
// "-", the ends alphanumeric, and a value over 63 is cut, with a hash of s
// so different long values stay apart.
func LabelValue(s string) string {
	notAlnum := func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') }
	v := strings.TrimFunc(labelBadRE.ReplaceAllString(s, "-"), notAlnum)
	if len(v) > 63 {
		sum := sha256.Sum256([]byte(s))
		v = strings.TrimRightFunc(v[:54], notAlnum) + "-" + hex.EncodeToString(sum[:4])
	}
	return v
}
