package internal

type ProjectPageData struct {
	Project        *Project
	Tab            ProjectTab
	Jobs           []*Job
	Visualizations []*Visualization
}
