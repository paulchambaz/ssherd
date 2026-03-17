package internal

type EventKind string

const (
	EventJobStatus   EventKind = "status"
	EventJobProgress EventKind = "progress"
	EventVizDone     EventKind = "viz_done"
	EventJobDeleted  EventKind = "deleted"
)

type JobEvent struct {
	Kind      EventKind
	Job       *Job
	StdoutLog string
	StderrLog string

	VizID       string
	ProjectID   string
	ProjectSlug string
	ComboKey    string
	VizErr      string
}
