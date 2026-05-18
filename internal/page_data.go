package internal

type ProjectPageData struct {
	Project        *Project
	Tab            ProjectTab
	Jobs           []*Job
	TotalJobs      int
	Page           int
	TotalPages     int
	Visualizations []*Visualization
	RepoFiles      []RepoFile
	RepoSubPath    string
}
