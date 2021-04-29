package queue

import (
	"context"
	"sync"
	"time"

	"github.com/centrifuge/go-centrifuge/errors"
	"github.com/centrifuge/gocelery"
	logging "github.com/ipfs/go-log"
)

// Constants are commonly used by all the tasks through kwargs.
const (
	TimeoutParam string = "Timeout"
)

var log = logging.Logger("queue-server")

// Config is an interface for queue specific configurations
type Config interface {

	// GetNumWorkers gets the number of background workers to initiate
	GetNumWorkers() int

	// GetWorkerWaitTime gets the worker wait time for a task to be available while polling
	// increasing this may slow down task execution while reducing it may consume a lot of CPU cycles
	GetWorkerWaitTimeMS() int

	// GetTaskValidDuration until which the task is valid from the creation
	GetTaskValidDuration() time.Duration
}

// TaskType is a task to be queued in the centrifuge node to be completed asynchronously
type TaskType interface {

	// TaskTypeName of the task
	TaskTypeName() string
}

// TaskResult represents a result from a queued task execution
type TaskResult interface {

	// Get the result within a timeout from the queue task execution
	Get(timeout time.Duration) (interface{}, error)
}

// Server represents the queue server currently implemented based on gocelery
type Server struct {
	config    Config
	lock      sync.RWMutex
	queue     *gocelery.CeleryClient
	taskTypes []TaskType
}

// Name of the queue server
func (qs *Server) Name() string {
	return "QueueServer"
}

// Start the queue server
func (qs *Server) Start(ctx context.Context, wg *sync.WaitGroup, startupErr chan<- error) {
	defer wg.Done()
	qs.lock.Lock()
	var err error
	qs.queue, err = gocelery.NewCeleryClient(
		gocelery.NewInMemoryBroker(),
		gocelery.NewInMemoryBackend(),
		qs.config.GetNumWorkers(),
		qs.config.GetWorkerWaitTimeMS(),
	)
	if err != nil {
		startupErr <- err
	}
	for _, task := range qs.taskTypes {
		qs.queue.Register(task.TaskTypeName(), task)
	}
	// start the workers
	qs.queue.StartWorker()
	qs.lock.Unlock()

	<-ctx.Done()
	log.Info("Shutting down Queue server with context done")
	qs.lock.Lock()
	qs.queue.StopWorker()
	qs.lock.Unlock()
	log.Info("Queue server stopped")
}

// RegisterTaskType registers a task type on the queue server
func (qs *Server) RegisterTaskType(name string, task interface{}) {
	qs.lock.Lock()
	defer qs.lock.Unlock()
	qs.taskTypes = append(qs.taskTypes, task.(TaskType))
}

// EnqueueJob enqueues a job on the queue server for the given taskTypeName
func (qs *Server) EnqueueJob(taskName string, params map[string]interface{}) (TaskResult, error) {
	qs.lock.RLock()
	defer qs.lock.RUnlock()
	settings := gocelery.DefaultSettings()
	settings.ValidUntil = time.Now().Add(qs.config.GetTaskValidDuration())
	return qs.enqueueJob(taskName, params, settings)
}

func (qs *Server) enqueueJob(name string, params map[string]interface{}, settings *gocelery.TaskSettings) (TaskResult, error) {
	if qs.queue == nil {
		return nil, errors.New("queue hasn't been initialised")
	}

	return qs.queue.Delay(gocelery.Task{
		Name:     name,
		Kwargs:   params,
		Settings: settings,
	})
}

// GetDuration parses key parameter to time.Duration type
func GetDuration(key interface{}) (time.Duration, error) {
	f64, ok := key.(float64)
	if !ok {
		return time.Duration(0), errors.New("Could not parse interface to float64")
	}
	return time.Duration(f64), nil
}

// TaskQueuer can be implemented by any queueing system
type TaskQueuer interface {
	EnqueueJob(taskTypeName string, params map[string]interface{}) (TaskResult, error)
}
