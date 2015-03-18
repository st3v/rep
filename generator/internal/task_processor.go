package internal

import (
	"github.com/cloudfoundry-incubator/executor"
	"github.com/cloudfoundry-incubator/rep"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/bbserrors"
	"github.com/pivotal-golang/lager"
)

const TaskCompletionReasonMissingContainer = "task container does not exist"
const TaskCompletionReasonFailedToRunContainer = "failed to run container"
const TaskCompletionReasonInvalidTransition = "invalid state transition"
const TaskCompletionReasonFailedToFetchResult = "failed to fetch result"

//go:generate counterfeiter -o fake_internal/fake_task_processor.go task_processor.go TaskProcessor

type TaskProcessor interface {
	Process(lager.Logger, executor.Container)
}

type taskProcessor struct {
	bbs               bbs.RepBBS
	containerDelegate ContainerDelegate
	cellID            string
}

func NewTaskProcessor(bbs bbs.RepBBS, containerDelegate ContainerDelegate, cellID string) TaskProcessor {
	return &taskProcessor{
		bbs:               bbs,
		containerDelegate: containerDelegate,
		cellID:            cellID,
	}
}

func (p *taskProcessor) Process(logger lager.Logger, container executor.Container) {
	logger = logger.Session("task-processor", lager.Data{
		"container-guid":  container.Guid,
		"container-state": container.State,
	})

	logger.Debug("starting")
	defer logger.Debug("finished")

	switch container.State {
	case executor.StateReserved:
		logger.Debug("processing-reserved-container")
		p.processActiveContainer(logger, container)
	case executor.StateInitializing:
		logger.Debug("processing-initializing-container")
		p.processActiveContainer(logger, container)
	case executor.StateCreated:
		logger.Debug("processing-created-container")
		p.processActiveContainer(logger, container)
	case executor.StateRunning:
		logger.Debug("processing-running-container")
		p.processActiveContainer(logger, container)
	case executor.StateCompleted:
		logger.Debug("processing-completed-container")
		p.processCompletedContainer(logger, container)
	}
}

func (p *taskProcessor) processActiveContainer(logger lager.Logger, container executor.Container) {
	ok := p.startTask(logger, container.Guid)
	if !ok {
		return
	}

	ok = p.containerDelegate.RunContainer(logger, container.Guid)
	if !ok {
		p.failTask(logger, container.Guid, TaskCompletionReasonFailedToRunContainer)
	}
}

func (p *taskProcessor) processCompletedContainer(logger lager.Logger, container executor.Container) {
	p.completeTask(logger, container)
	p.containerDelegate.DeleteContainer(logger, container.Guid)
}

func (p *taskProcessor) startTask(logger lager.Logger, guid string) bool {
	logger.Info("starting-task")
	changed, err := p.bbs.StartTask(logger, guid, p.cellID)
	if err != nil {
		logger.Error("failed-starting-task", err)

		if _, ok := err.(bbserrors.TaskStateTransitionError); ok {
			p.containerDelegate.DeleteContainer(logger, guid)
		} else if err == bbserrors.ErrTaskRunningOnDifferentCell {
			p.containerDelegate.DeleteContainer(logger, guid)
		} else if err == bbserrors.ErrStoreResourceNotFound {
			p.containerDelegate.DeleteContainer(logger, guid)
		}

		return false
	}

	if changed {
		logger.Info("succeeded-starting-task")
	} else {
		logger.Info("task-already-started")
	}

	return changed
}

func (p *taskProcessor) completeTask(logger lager.Logger, container executor.Container) {
	var result string
	var err error
	if !container.RunResult.Failed {
		result, err = p.containerDelegate.FetchContainerResultFile(logger, container.Guid, container.Tags[rep.ResultFileTag])
		if err != nil {
			p.failTask(logger, container.Guid, TaskCompletionReasonFailedToFetchResult)
			return
		}
	}

	logger.Info("completing-task")
	err = p.bbs.CompleteTask(logger, container.Guid, p.cellID, container.RunResult.Failed, container.RunResult.FailureReason, result)
	if err != nil {
		logger.Error("failed-completing-task", err)

		if _, ok := err.(bbserrors.TaskStateTransitionError); ok {
			p.failTask(logger, container.Guid, TaskCompletionReasonInvalidTransition)
		}
		return
	}

	logger.Info("succeeded-completing-task")
}

func (p *taskProcessor) failTask(logger lager.Logger, guid string, reason string) {
	logger.Info("failing-task")
	err := p.bbs.FailTask(logger, guid, reason)
	if err != nil {
		logger.Error("failed-failing-task", err)
		return
	}

	logger.Info("succeeded-failing-task")
}
