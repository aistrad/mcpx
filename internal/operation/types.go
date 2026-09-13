package operation

import (
	"context"
	"encoding/json"
	"time"
)

// State is the durable lifecycle state of an asynchronous operation or step.
type State string

const (
	StateQueued              State = "queued"
	StateRunning             State = "running"
	StateSucceeded           State = "succeeded"
	StateFailed              State = "failed"
	StateWaitingConfirmation State = "waiting_confirmation"
	StateInterrupted         State = "interrupted"
	StateCancelled           State = "cancelled"
	StateSkipped             State = "skipped"
)

func (s State) terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateInterrupted || s == StateCancelled
}

// StepSpec describes one public tool invocation in a batch.
type StepSpec struct {
	ID        string
	Tool      string
	Arguments map[string]any
	DependsOn []string
	Exclusive bool
}

// SubmitSpec describes an operation and all of its steps.
type SubmitSpec struct {
	RunID           string
	ID              string
	RemoteSessionID string
	WorkspaceName   string
	RequestID       string
	Purpose         string
	Steps           []StepSpec
}

// Record is the public operation state assembled from durable records.
type Record struct {
	RunID           string
	StateSequence   int64
	StateEventID    string
	ID              string
	RemoteSessionID string
	WorkspaceName   string
	RequestID       string
	Purpose         string
	State           State
	Result          json.RawMessage
	Error           json.RawMessage
	CreatedAt       time.Time
	StartedAt       *time.Time
	CompletedAt     *time.Time
	Steps           []StepRecord
}

// StepRecord is the durable state and result of one operation step.
type StepRecord struct {
	ID                string
	Tool              string
	Arguments         map[string]any
	RequestID         string
	DependsOn         []string
	Exclusive         bool
	State             State
	Result            json.RawMessage
	Error             json.RawMessage
	ConfirmationToken string
	CreatedAt         time.Time
	StartedAt         *time.Time
	CompletedAt       *time.Time
}

// ResultPage contains an operation record and an optionally paged result.
type ResultPage struct {
	Operation  Record
	StepID     string
	Result     json.RawMessage
	NextCursor string
}

// ExecuteInput is the context passed to the Runtime adapter for one step.
type ExecuteInput struct {
	OperationID     string
	StepID          string
	RequestID       string
	RemoteSessionID string
	WorkspaceName   string
	Purpose         string
	Tool            string
	Arguments       map[string]any
}

// ExecuteResult is returned by the Runtime adapter after invoking a tool.
type ExecuteResult struct {
	Result              json.RawMessage
	WaitingConfirmation bool
	ConfirmationToken   string
	Err                 error
}

// Executor invokes one public tool step.
type Executor func(context.Context, ExecuteInput) ExecuteResult

// Event is emitted after an operation lifecycle transition.
type Event struct {
	RunID           string
	StateSequence   int64
	StateEventID    string
	OperationID     string
	StepID          string
	RemoteSessionID string
	WorkspaceName   string
	RequestID       string
	Tool            string
	Type            string
	State           State
	Summary         string
	CreatedAt       time.Time
}

// EventSink receives lifecycle events without affecting operation execution.
type EventSink func(Event)
