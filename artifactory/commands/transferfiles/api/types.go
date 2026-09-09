package api

type ProcessStatusType string
type ChunkFileStatusType string

const (
	Done       ProcessStatusType = "DONE"
	InProgress ProcessStatusType = "IN_PROGRESS"

	Success             ChunkFileStatusType = "SUCCESS"
	Fail                ChunkFileStatusType = "FAIL"
	SkippedLargeProps   ChunkFileStatusType = "SKIPPED_LARGE_PROPS"
	SkippedMetadataFile ChunkFileStatusType = "SKIPPED_METADATA_FILE"
	SkippedNonEmptyDir  ChunkFileStatusType = "SKIPPED_NON_EMPTY_DIR"

	Phase1 int = 0
	Phase2 int = 1
	Phase3 int = 2
)

type FileRepresentation struct {
	Repo        string `json:"repo,omitempty"`
	Path        string `json:"path,omitempty"`
	Name        string `json:"name,omitempty"`
	Size        int64  `json:"size,omitempty"`
	NonEmptyDir bool   `json:"non_empty_dir,omitempty"`
}

type ChunkStatus struct {
	Status ProcessStatusType          `json:"status,omitempty"`
	Files  []FileUploadStatusResponse `json:"files,omitempty"`
}

type FileUploadStatusResponse struct {
	FileRepresentation
	SizeBytes        int64               `json:"size_bytes,omitempty"`
	ChecksumDeployed bool                `json:"checksum_deployed,omitempty"`
	Status           ChunkFileStatusType `json:"status,omitempty"`
	StatusCode       int                 `json:"status_code,omitempty"`
	Reason           string              `json:"reason,omitempty"`
}
