package internal

type EventKind string

const (
	EventJobStatus   EventKind = "status"
	EventJobProgress EventKind = "progress"
)

type JobEvent struct {
	Kind      EventKind
	Job       *Job
	StdoutLog string
	StderrLog string
}
