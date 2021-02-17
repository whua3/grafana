package job

import (
	"sync"

	"github.com/grafana/grafana/pkg/services/alerting/rule"
)

// Job holds state about when the alert rule should be evaluated.
type Job struct {
	Offset      int64
	OffsetWait  bool
	Delay       bool
	running     bool
	Rule        *rule.Rule
	runningLock sync.Mutex // Lock for running property which is used in the Scheduler and AlertEngine execution
}

// GetRunning returns true if the job is running. A lock is taken and released on the Job to ensure atomicity.
func (j *Job) GetRunning() bool {
	defer j.runningLock.Unlock()
	j.runningLock.Lock()
	return j.running
}

// SetRunning sets the running property on the Job. A lock is taken and released on the Job to ensure atomicity.
func (j *Job) SetRunning(b bool) {
	j.runningLock.Lock()
	j.running = b
	j.runningLock.Unlock()
}