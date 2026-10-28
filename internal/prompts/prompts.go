// Package prompts holds the role prompts, versioned with the harness.
package prompts

import (
	"bytes"
	_ "embed"
	"text/template"
)

//go:embed implement.md
var implement string

//go:embed review.md
var review string

//go:embed plan.md
var plan string

var (
	implementT = template.Must(template.New("implement").Parse(implement))
	reviewT    = template.Must(template.New("review").Parse(review))
	planT      = template.Must(template.New("plan").Parse(plan))
)

type Vars struct {
	Branch string
	Base   string
	SHA    string
	Fix    bool
	// Carried is a fix round on a PR devbot carries for someone: its branch
	// is theirs.
	Carried bool
	// Supersedes is the run that opens a PR from a branch of devbot's own
	// to carry on a fork's PR it can't push to.
	Supersedes bool
	// Protected are the paths the implementer must leave alone, and
	// Approvable the protected ones it may change, for a person to approve.
	Protected  string
	Approvable string
	// MCPServers are the run's MCP servers, one line each.
	MCPServers []string
	// The bounds on the notes an agent sends other tasks.
	NoteMax, NotesPerRun int
	// Deps is where an implement run has the unmerged work of the task it
	// builds against checked out, when it has one.
	Deps string
	// BuiltAgainst, on a review, is the repos of the tasks the PR's task
	// built against while their work was unmerged, when there are any.
	BuiltAgainst string
	// Contract is set for a task whose parent task's approved plan has a
	// contract, which .devbot/context.md holds.
	Contract bool
	// Replan, on a plan job, plans a parent task again over the children
	// it has; Repos are the candidate repos, by their full refs, and
	// MaxChildren the most children a plan may have.
	Replan      bool
	Repos       []string
	MaxChildren int
}

func Implement(v Vars) string { return render(implementT, v) }
func Review(v Vars) string    { return render(reviewT, v) }
func Plan(v Vars) string      { return render(planT, v) }

func render(t *template.Template, v Vars) string {
	var b bytes.Buffer
	if err := t.Execute(&b, v); err != nil {
		panic(err) // templates are compiled in; a failure is a bug
	}
	return b.String()
}
