// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"go.temporal.io/server/api/historyservice/v1"
	"go.temporal.io/server/api/matchingservice/v1"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/visibility/manager"
	"go.temporal.io/server/service/history/configs"
	"go.temporal.io/server/service/history/queues"
	"go.temporal.io/server/service/history/shard"
	"go.temporal.io/server/service/history/tasks"
	"go.temporal.io/server/service/history/workflow"
)

type (
	updateVisibilityAckLevel func(ackLevel int64) error
	visibilityQueueShutdown  func() error

	visibilityQueueProcessorImpl struct {
		// from transferQueueActiveProcessorImpl (transferQueueProcessorImpl.activeTaskProcessor)
		*queueProcessorBase
		queueAckMgr
		shard                    shard.Context
		options                  *QueueProcessorOptions
		executionManager         persistence.ExecutionManager
		maxReadAckLevel          maxReadAckLevel
		updateVisibilityAckLevel updateVisibilityAckLevel
		visibilityQueueShutdown  visibilityQueueShutdown
		visibilityTaskFilter     taskFilter
		logger                   log.Logger
		metricsClient            metrics.Client
		taskExecutor             queueTaskExecutor

		// from transferQueueProcessorImpl
		config   *configs.Config
		ackLevel int64

		isStarted    int32
		isStopped    int32
		shutdownChan chan struct{}
	}
)

func newVisibilityQueueProcessor(
	shard shard.Context,
	workflowCache workflow.Cache,
	visibilityMgr manager.VisibilityManager,
	matchingClient matchingservice.MatchingServiceClient,
	historyClient historyservice.HistoryServiceClient,
) queues.Processor {

	config := shard.GetConfig()
	logger := log.With(shard.GetLogger(), tag.ComponentVisibilityQueue)
	metricsClient := shard.GetMetricsClient()

	options := &QueueProcessorOptions{
		BatchSize:                           config.VisibilityTaskBatchSize,
		WorkerCount:                         config.VisibilityTaskWorkerCount,
		MaxPollRPS:                          config.VisibilityProcessorMaxPollRPS,
		MaxPollInterval:                     config.VisibilityProcessorMaxPollInterval,
		MaxPollIntervalJitterCoefficient:    config.VisibilityProcessorMaxPollIntervalJitterCoefficient,
		UpdateAckInterval:                   config.VisibilityProcessorUpdateAckInterval,
		UpdateAckIntervalJitterCoefficient:  config.VisibilityProcessorUpdateAckIntervalJitterCoefficient,
		MaxRetryCount:                       config.VisibilityTaskMaxRetryCount,
		RedispatchInterval:                  config.VisibilityProcessorRedispatchInterval,
		RedispatchIntervalJitterCoefficient: config.VisibilityProcessorRedispatchIntervalJitterCoefficient,
		MaxRedispatchQueueSize:              config.VisibilityProcessorMaxRedispatchQueueSize,
		EnablePriorityTaskProcessor:         config.VisibilityProcessorEnablePriorityTaskProcessor,
		MetricScope:                         metrics.VisibilityQueueProcessorScope,
	}
	visibilityTaskFilter := func(taskInfo tasks.Task) (bool, error) {
		return true, nil
	}
	maxReadAckLevel := func() int64 {
		return shard.GetQueueMaxReadLevel(
			tasks.CategoryVisibility,
			shard.GetClusterMetadata().GetCurrentClusterName(),
		).TaskID
	}
	updateVisibilityAckLevel := func(ackLevel int64) error {
		return shard.UpdateQueueAckLevel(tasks.CategoryVisibility, tasks.Key{TaskID: ackLevel})
	}

	visibilityQueueShutdown := func() error {
		return nil
	}

	ackLevel := shard.GetQueueAckLevel(tasks.CategoryVisibility).TaskID
	retProcessor := &visibilityQueueProcessorImpl{
		shard:                    shard,
		options:                  options,
		maxReadAckLevel:          maxReadAckLevel,
		updateVisibilityAckLevel: updateVisibilityAckLevel,
		visibilityQueueShutdown:  visibilityQueueShutdown,
		visibilityTaskFilter:     visibilityTaskFilter,
		logger:                   logger,
		metricsClient:            metricsClient,
		taskExecutor: newVisibilityQueueTaskExecutor(
			shard,
			workflowCache,
			visibilityMgr,
			logger,
		),

		config:       config,
		ackLevel:     ackLevel,
		shutdownChan: make(chan struct{}),

		queueAckMgr:        nil, // is set bellow
		queueProcessorBase: nil, // is set bellow
		executionManager:   shard.GetExecutionManager(),
	}

	queueAckMgr := newQueueAckMgr(
		shard,
		options,
		retProcessor,
		ackLevel,
		logger,
	)

	queueProcessorBase := newQueueProcessorBase(
		shard.GetClusterMetadata().GetCurrentClusterName(),
		shard,
		options,
		retProcessor,
		queueAckMgr,
		workflowCache,
		logger,
		shard.GetMetricsClient().Scope(metrics.VisibilityQueueProcessorScope),
	)
	retProcessor.queueAckMgr = queueAckMgr
	retProcessor.queueProcessorBase = queueProcessorBase

	return retProcessor
}

// visibilityQueueProcessor implementation
func (t *visibilityQueueProcessorImpl) Start() {
	if !atomic.CompareAndSwapInt32(&t.isStarted, 0, 1) {
		return
	}
	t.queueProcessorBase.Start()
	go t.completeTaskLoop()
}

func (t *visibilityQueueProcessorImpl) Stop() {
	if !atomic.CompareAndSwapInt32(&t.isStopped, 0, 1) {
		return
	}
	t.queueProcessorBase.Stop()
	close(t.shutdownChan)
}

// NotifyNewTasks - Notify the processor about the new visibility task arrival.
// This should be called each time new visibility task arrives, otherwise tasks maybe delayed.
func (t *visibilityQueueProcessorImpl) NotifyNewTasks(
	_ string,
	visibilityTasks []tasks.Task,
) {
	if len(visibilityTasks) != 0 {
		t.notifyNewTask()
	}
}

func (t *visibilityQueueProcessorImpl) FailoverNamespace(
	namespaceIDs map[string]struct{},
) {
	// no-op
}

func (t *visibilityQueueProcessorImpl) LockTaskProcessing() {
	// no-op
}

func (t *visibilityQueueProcessorImpl) UnlockTaskProcessing() {
	// no-op
}

func (t *visibilityQueueProcessorImpl) Category() tasks.Category {
	return tasks.CategoryVisibility
}

func (t *visibilityQueueProcessorImpl) completeTaskLoop() {
	timer := time.NewTimer(t.config.VisibilityProcessorCompleteTaskInterval())
	defer timer.Stop()

	for {
		select {
		case <-t.shutdownChan:
			// before shutdown, make sure the ack level is up to date
			err := t.completeTask()
			if err != nil {
				t.logger.Error("Error complete visibility task", tag.Error(err))
			}
			return
		case <-timer.C:
			for attempt := 1; attempt <= t.config.VisibilityProcessorCompleteTaskFailureRetryCount(); attempt++ {
				err := t.completeTask()
				if err == nil {
					break
				}

				t.logger.Info("Failed to complete visibility task", tag.Error(err))
				if errors.Is(err, shard.ErrShardClosed) {
					// shard closed, trigger shutdown and bail out
					t.Stop()
					return
				}
				backoff := time.Duration((attempt-1)*100) * time.Millisecond
				time.Sleep(backoff)
			}
			timer.Reset(t.config.VisibilityProcessorCompleteTaskInterval())
		}
	}
}

func (t *visibilityQueueProcessorImpl) completeTask() error {
	lowerAckLevel := t.ackLevel
	upperAckLevel := t.queueAckMgr.getQueueAckLevel()

	t.logger.Debug("Start completing visibility task", tag.AckLevel(lowerAckLevel), tag.AckLevel(upperAckLevel))
	if lowerAckLevel >= upperAckLevel {
		return nil
	}

	t.metricsClient.IncCounter(metrics.VisibilityQueueProcessorScope, metrics.TaskBatchCompleteCounter)

	if lowerAckLevel < upperAckLevel {
		err := t.shard.GetExecutionManager().RangeCompleteHistoryTasks(&persistence.RangeCompleteHistoryTasksRequest{
			ShardID:      t.shard.GetShardID(),
			TaskCategory: tasks.CategoryVisibility,
			MinTaskKey: tasks.Key{
				TaskID: lowerAckLevel,
			},
			MaxTaskKey: tasks.Key{
				TaskID: upperAckLevel,
			},
		})
		if err != nil {
			return err
		}
	}

	t.ackLevel = upperAckLevel

	return t.shard.UpdateQueueAckLevel(tasks.CategoryVisibility, tasks.Key{TaskID: upperAckLevel})
}

// queueProcessor interface
func (t *visibilityQueueProcessorImpl) notifyNewTask() {
	t.queueProcessorBase.notifyNewTask()
}

// taskExecutor interfaces
func (t *visibilityQueueProcessorImpl) getTaskFilter() taskFilter {
	return t.visibilityTaskFilter
}

func (t *visibilityQueueProcessorImpl) complete(
	taskInfo *taskInfo,
) {

	t.queueProcessorBase.complete(taskInfo.Task)
}

func (t *visibilityQueueProcessorImpl) process(
	ctx context.Context,
	taskInfo *taskInfo,
) (int, error) {
	// TODO: task metricScope should be determined when creating taskInfo
	metricScope := getVisibilityTaskMetricsScope(taskInfo.Task)
	return metricScope, t.taskExecutor.execute(ctx, taskInfo.Task, taskInfo.shouldProcessTask)
}

// processor interfaces
func (t *visibilityQueueProcessorImpl) readTasks(
	readLevel int64,
) ([]tasks.Task, bool, error) {

	response, err := t.executionManager.GetHistoryTasks(&persistence.GetHistoryTasksRequest{
		ShardID:      t.shard.GetShardID(),
		TaskCategory: tasks.CategoryVisibility,
		MinTaskKey: tasks.Key{
			TaskID: readLevel,
		},
		MaxTaskKey: tasks.Key{
			TaskID: t.maxReadAckLevel(),
		},
		BatchSize: t.options.BatchSize(),
	})

	if err != nil {
		return nil, false, err
	}

	return response.Tasks, len(response.NextPageToken) != 0, nil
}

func (t *visibilityQueueProcessorImpl) updateAckLevel(
	ackLevel int64,
) error {

	return t.updateVisibilityAckLevel(ackLevel)
}

func (t *visibilityQueueProcessorImpl) queueShutdown() error {
	return t.visibilityQueueShutdown()
}

// some aux stuff
func getVisibilityTaskMetricsScope(
	task tasks.Task,
) int {
	switch task.(type) {
	case *tasks.StartExecutionVisibilityTask:
		return metrics.VisibilityTaskStartExecutionScope
	case *tasks.UpsertExecutionVisibilityTask:
		return metrics.VisibilityTaskUpsertExecutionScope
	case *tasks.CloseExecutionVisibilityTask:
		return metrics.VisibilityTaskCloseExecutionScope
	case *tasks.DeleteExecutionVisibilityTask:
		return metrics.VisibilityTaskDeleteExecutionScope
	default:
		return metrics.VisibilityQueueProcessorScope
	}
}
