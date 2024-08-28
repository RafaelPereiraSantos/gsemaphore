package gsemaphore

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

type (
	Semaphore[T any] struct {
		f                               pipeline[T]
		startingParallelPipelinesAmount int
		errorsChannel                   chan error

		semaphore chan struct{}

		workersPool sync.Pool

		parallelismStrategy goroutinesRampUpStrategy[T]
	}

	worker struct {
		id string
	}

	pipeline[T any] func(T, context.Context) error

	OptionFunc[T any] func(*Semaphore[T]) *Semaphore[T]

	WorkerKey string

	goroutinesRampUpStrategy[T any] func(context.Context, *Semaphore[T]) (strategyFollowUp, context.CancelFunc)
	strategyFollowUp                func()
)

const (
	defaultParallelismAmount           = 1
	WorkerIDcontextKey       WorkerKey = "swid"
)

// NewSemaphore returns a new Semaphore properly configured with the given options.
func NewSemaphore[T any](options []OptionFunc[T]) *Semaphore[T] {
	sem := &Semaphore[T]{
		workersPool: sync.Pool{
			New: func() any {
				return &worker{
					id: uuid.NewString(),
				}
			},
		},
	}

	for _, opt := range options {
		sem = opt(sem)
	}

	return sem
}

// Run initialize the async processing of each item with the given pipeline function. It must receive a context which
// will be used to controll dead lines.
// If the startingParallelPipelinesAmount was greater than zero, the async processing will start with the amount of
// goroutines equal to the startingParallelPipelinesAmount attribute and slowly increase its capacity up to
// maxParallelPipelinesAmount, incrementing 1 extra goroutine each timeBetweenParallelismIncrease.
func (sem *Semaphore[T]) Run(ctx context.Context, itemsToProcess []T) {
	followUp, followUpCancel := sem.parallelismStrategy(ctx, sem)

	if followUp != nil {
		go followUp()
	}

	if followUpCancel != nil {
		defer followUpCancel()
	}

	semaphoreWG := sync.WaitGroup{}

	for _, itemToProcess := range itemsToProcess {
		semaphoreWG.Add(1)
		sem.semaphore <- struct{}{}
		worker := sem.workersPool.Get().(*worker)

		go func(pipe pipeline[T], item T, errChan chan error) {
			defer semaphoreWG.Done()
			defer func() {
				<-sem.semaphore
			}()
			defer sem.workersPool.Put(worker)

			ctxWithWorker := context.WithValue(ctx, WorkerIDcontextKey, worker.id)
			if err := sem.f(item, ctxWithWorker); err != nil {
				errChan <- err
			}

		}(sem.f, itemToProcess, sem.errorsChannel)
	}

	semaphoreWG.Wait()
	close(sem.errorsChannel)
}

// UpdateSettings allows that a already instantiated semaphore to be updated, it accepts any function with the signature
// of OptionFunc[T].
func (sem *Semaphore[T]) UpdateSettings(options []OptionFunc[T]) {
	for _, opts := range options {
		opts(sem)
	}
}

// WithPipeline allows a pipeline to be passed to the semaphore that will be in charge of precessing each T element
// inside the list of itens to process.
func WithPipeline[T any](f pipeline[T]) OptionFunc[T] {
	return func(s *Semaphore[T]) *Semaphore[T] {
		s.f = f

		return s
	}
}

// WithErrorChannel is a function that allows that a channel be passed to the semaphore allowing that the semaphore
// send errors that happened with the pipeline.
func WithErrorChannel[T any](errorsChannel chan error) OptionFunc[T] {
	return func(s *Semaphore[T]) *Semaphore[T] {
		s.errorsChannel = errorsChannel

		return s
	}
}

// WithParallelismStrategyOf allows that the number of goroutines running at same time be defined following any strategy
// from maxing out from the very beginning with all goroutines running or a slow increase over time.
func WithParallelismStrategyOf[T any](str goroutinesRampUpStrategy[T]) OptionFunc[T] {
	return func(s *Semaphore[T]) *Semaphore[T] {
		if str == nil {
			return s
		}

		s.parallelismStrategy = str

		return s
	}
}

// BuildLinearParallelismIncreaseStrategy creates a strategy function that follows the linear progression of goroutines
// increase.
func BuildLinearParallelismIncreaseStrategy[T any](
	startingParallelPipelinesAmount int,
	maxParallelPipelinesAmount int,
	timeBetweenParallelismIncrease time.Duration,
) goroutinesRampUpStrategy[T] {
	return func(
		ctx context.Context,
		sem *Semaphore[T],
	) (strategyFollowUp, context.CancelFunc) {
		sem.semaphore = make(chan struct{}, maxParallelPipelinesAmount)

		for i := 0; i < maxParallelPipelinesAmount-sem.startingParallelPipelinesAmount-1; i++ {
			sem.semaphore <- struct{}{}
		}

		ctxForNewSlots, cancel := context.WithCancel(ctx)

		linearSpotsIncreaser := func() {
			ticker := time.NewTicker(timeBetweenParallelismIncrease)
			defer ticker.Stop()

			currentParallelPipelinesAmount := sem.startingParallelPipelinesAmount

			for {
				select {
				case <-ctxForNewSlots.Done():
					return
				case <-ticker.C:
					<-sem.semaphore
					currentParallelPipelinesAmount++

					if currentParallelPipelinesAmount >= maxParallelPipelinesAmount {
						return
					}
				}
			}
		}

		return linearSpotsIncreaser, cancel
	}
}

// BuildFullCapacityFromStartStrategy creates a strategy function that enables all goroutines to run from the very
// beginning.
func BuildFullCapacityFromStartStrategy[T any](maxParallelPipelinesAmount int) goroutinesRampUpStrategy[T] {
	return func(_ context.Context, sem *Semaphore[T]) (strategyFollowUp, context.CancelFunc) {
		sem.semaphore = make(chan struct{}, maxParallelPipelinesAmount)

		return nil, nil
	}
}
