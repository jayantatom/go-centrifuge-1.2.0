package jobsv1

import (
	"context"
	"fmt"
	"time"

	"github.com/centrifuge/go-centrifuge/errors"
	"github.com/centrifuge/go-centrifuge/identity"
	"github.com/centrifuge/go-centrifuge/jobs"
	"github.com/centrifuge/go-centrifuge/notification"
)

const (
	managerLogPrefix = "manager"
)

// NewManager returns a JobManager implementation.
func NewManager(config jobs.Config, repo jobs.Repository) jobs.Manager {
	return &manager{config: config, repo: repo, notifier: notification.NewWebhookSender()}
}

// manager implements JobManager.
// TODO [JobManager] convert this into an implementation of node.Server and start it at node start so that we can bring down transaction go routines cleanly
type manager struct {
	config   jobs.Config
	repo     jobs.Repository
	notifier notification.Sender
}

func (s *manager) GetDefaultTaskTimeout() time.Duration {
	return s.config.GetTaskValidDuration()
}

func (s *manager) UpdateJobWithValue(accountID identity.DID, id jobs.JobID, key string, value []byte) error {
	tx, err := s.GetJob(accountID, id)
	if err != nil {
		return err
	}
	tx.Values[key] = jobs.JobValue{Key: key, Value: value}
	return s.saveJob(tx)
}

func (s *manager) UpdateTaskStatus(accountID identity.DID, id jobs.JobID, status jobs.Status, taskName, message string) error {
	tx, err := s.GetJob(accountID, id)
	if err != nil {
		return err
	}

	// status particular to the task
	tx.TaskStatus[taskName] = status
	tx.Logs = append(tx.Logs, jobs.NewLog(taskName, message))
	return s.saveJob(tx)
}

// ExecuteWithinJob executes a task within a Job.
func (s *manager) ExecuteWithinJob(ctx context.Context, accountID identity.DID, existingJobID jobs.JobID, desc string, work func(accountID identity.DID, txID jobs.JobID, txMan jobs.Manager, err chan<- error)) (txID jobs.JobID, done chan error, err error) {
	job, err := s.repo.Get(accountID, existingJobID)
	if err != nil {
		job = jobs.NewJob(accountID, desc)
		err := s.saveJob(job)
		if err != nil {
			return jobs.NilJobID(), nil, err
		}
	}
	// set capacity to one so that any late listener won't block this routine.
	done = make(chan error, 1)
	go func(ctx context.Context) {
		err := make(chan error)
		go work(accountID, job.ID, s, err)

		var mJob *jobs.Job
		var doneErr error
		select {
		case e := <-err:
			tempJob, err := s.repo.Get(accountID, job.ID)
			if err != nil {
				log.Error(e, err)
				doneErr = errors.AppendError(e, err)
				break
			}
			// update job success status only if this wasn't an existing job.
			// Otherwise it might update an existing tx pending status to success without actually being a success,
			// It is assumed that status update is already handled per task in that case.
			// Checking individual task success is upto the transaction manager users.
			if e == nil && jobs.JobIDEqual(existingJobID, jobs.NilJobID()) {
				tempJob.Status = jobs.Success
			} else if e != nil {
				log.Error(e)
				action := fmt.Sprintf("%s[%s]", managerLogPrefix, desc)
				doneErr = fmt.Errorf(fmt.Sprintf("%s %s", action, e.Error()))
				tempJob.Logs = append(tempJob.Logs, jobs.NewLog(action, e.Error()))
				tempJob.Status = jobs.Failed
			}
			es := s.saveJob(tempJob)
			if es != nil {
				log.Error(e, es)
				doneErr = errors.AppendError(e, es)
			}
			mJob = tempJob
		case <-ctx.Done():
			msg := fmt.Sprintf("Job %s for account %s with description \"%s\" is stopped because of context close", job.ID.String(), job.DID, job.Description)
			log.Warningf(msg)
			tempJob, err := s.repo.Get(accountID, job.ID)
			if err != nil {
				log.Error(err)
				doneErr = err
				break
			}
			tempJob.Logs = append(tempJob.Logs, jobs.NewLog("context closed", msg))
			e := s.saveJob(tempJob)
			if e != nil {
				log.Error(e)
				doneErr = e
			}
			mJob = tempJob
		}

		// non blocking send
		select {
		case done <- doneErr:
		default:
			// must not happen
			log.Error("job done channel capacity breach")
		}

		if mJob != nil && jobs.JobIDEqual(existingJobID, jobs.NilJobID()) {
			notificationMsg := notification.Message{
				EventType:    notification.JobCompleted,
				AccountID:    accountID.String(),
				Recorded:     time.Now().UTC(),
				DocumentType: jobs.JobDataTypeURL,
				DocumentID:   mJob.ID.String(),
				Status:       string(mJob.Status),
			}
			if len(mJob.Logs) > 0 {
				notificationMsg.Message = mJob.Logs[len(mJob.Logs)-1].Message
			}
			// Send Job notification webhook
			_, err := s.notifier.Send(ctx, notificationMsg)
			if err != nil {
				log.Error(err)
			}
		}

	}(ctx)
	return job.ID, done, nil
}

// saveJob saves the transaction.
func (s *manager) saveJob(tx *jobs.Job) error {
	err := s.repo.Save(tx)
	if err != nil {
		return err
	}
	return nil
}

// GetJob returns the job associated with identity and id.
func (s *manager) GetJob(accountID identity.DID, id jobs.JobID) (*jobs.Job, error) {
	return s.repo.Get(accountID, id)
}

// createJob creates a new job and saves it to the DB.
func (s *manager) createJob(accountID identity.DID, desc string) (*jobs.Job, error) {
	job := jobs.NewJob(accountID, desc)
	return job, s.saveJob(job)
}

// WaitForJob blocks until job status is moved from pending state.
// Note: use it with caution as this will block.
func (s *manager) WaitForJob(accountID identity.DID, txID jobs.JobID) error {
	// TODO change this to use a pre-saved done channel from ExecuteWithinJob, instead of a for loop, may require significant refactoring to handle the case of restarted node
	for {
		resp, err := s.GetJobStatus(accountID, txID)
		if err != nil {
			return err
		}

		switch jobs.Status(resp.Status) {
		case jobs.Failed:
			return errors.New("job failed: %v", resp.Message)
		case jobs.Success:
			return nil
		default:
			time.Sleep(10 * time.Millisecond)
			continue
		}
	}
}

// GetJobStatus returns the job status associated with identity and id.
func (s *manager) GetJobStatus(accountID identity.DID, id jobs.JobID) (resp jobs.StatusResponse, err error) {
	job, err := s.GetJob(accountID, id)
	if err != nil {
		return resp, err
	}

	var msg string
	lastUpdated := job.CreatedAt.UTC()
	if len(job.Logs) > 0 {
		log := job.Logs[len(job.Logs)-1]
		msg = log.Message
		lastUpdated = log.CreatedAt.UTC()
	}

	return jobs.StatusResponse{
		JobID:       job.ID.String(),
		Status:      string(job.Status),
		Message:     msg,
		LastUpdated: lastUpdated,
	}, nil
}
