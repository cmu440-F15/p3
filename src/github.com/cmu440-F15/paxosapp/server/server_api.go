// DO NOT MODIFY!

package dttserver

import "DTT/rpc/dttrpc"

type Method int

const (
	AddSuperJob Method = iota + 1
	SetTranscription
	AddCodeJob
	SetOptimize
	Noop
)

type LogEntry struct {
	Type      Method
	EncapData []byte
}

type SuperJobVal struct {
	JobsList []string
}

type TranscriptVal struct {
	Sid        int
	Jid        int
	Loc        string
	Transcript string
}

type CodeJob struct {
	Jid     int
	Title   string
	OldCode string
	NewCode string
	Name    string
	Time    float64
}

// DttServer defines the set of methods that a DttClient can invoke remotely via RPCs.
type DttServer interface {

	// Adds a new image to the database.
	// Replies with status Exists if the image job has previously been created.
	NewSuperJob(imgArg []string) error

	// Requesters call this method to get all its image jobs and their transcriptions
	GetSjobJobs(sjid int) ([]*dttrpc.Job, error)

	// Clients call this image to request an image for processing
	GetImage() (*dttrpc.Job, error)

	// Store uploaded transcription
	SetTranscription(job *dttrpc.Job) error

	// code optimizer functions
	NewCodeJob(job, title string) error

	GetAllCodeJobs() ([]*CodeJob, error)

	GetCode(codeID int) (*CodeJob, error)

	SetOptimize(code *CodeJob) error

	// general functions
	GetJobList(sjid int) ([]int, error)

	GetAllSjobs() ([]int, error)

	GetLog() map[int]*LogEntry
}
