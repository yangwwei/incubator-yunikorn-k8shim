/*
 Licensed to the Apache Software Foundation (ASF) under one
 or more contributor license agreements.  See the NOTICE file
 distributed with this work for additional information
 regarding copyright ownership.  The ASF licenses this file
 to you under the Apache License, Version 2.0 (the
 "License"); you may not use this file except in compliance
 with the License.  You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package cache

import (
	"fmt"
	"sort"
	"sync"

	"github.com/apache/incubator-yunikorn-k8shim/pkg/client"
	"github.com/looplab/fsm"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/apache/incubator-yunikorn-core/pkg/api"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/appmgmt/interfaces"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/constants"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/events"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/conf"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/dispatcher"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/log"
	"github.com/apache/incubator-yunikorn-scheduler-interface/lib/go/si"
)

type Application struct {
	applicationID string
	queue         string
	partition     string
	user          string
	taskMap       map[string]*Task
	reservationMapping map[string]*Task // actual -> mapping
	taskGroups    map[string]si.TaskGroup
	tags          map[string]string
	sm            *fsm.FSM
	lock          *sync.RWMutex
	schedulerAPI  api.SchedulerAPI
	clients       *client.Clients
}

func (app *Application) String() string {
	return fmt.Sprintf("applicationID: %s, queue: %s, partition: %s,"+
		" totalNumOfTasks: %d, currentState: %s",
		app.applicationID, app.queue, app.partition, len(app.taskMap), app.GetApplicationState())
}

func NewApplicationWithGang(appID, queueName, user string, tags map[string]string, taskGroups map[string]si.TaskGroup, clients *client.Clients) *Application {
	app := NewApplication(appID, queueName, user, tags, clients.SchedulerAPI)
	app.taskGroups = taskGroups
	app.clients = clients
	app.reservationMapping = make(map[string]*Task, 0)

	for tgName, tg := range taskGroups {
		log.Logger().Info("taskGroup",
			zap.String("name", tgName),
			zap.Int32("minMember", tg.MinMember),
			zap.Any("minResource", tg.MinResource))

		for i:=int32(0); i<tg.MinMember; i++ {
			podReservationName := "tg-" + tgName + "-" + app.applicationID + fmt.Sprintf("%d", i)

			// parsing resources
			var cpuResource resource.Quantity
			var memResource resource.Quantity
			if cpuStr, ok := tg.MinResource[v1.ResourceCPU.String()]; ok {
				cpuResource = resource.MustParse(cpuStr)
			}
			if memStr, ok := tg.MinResource[v1.ResourceMemory.String()]; ok {
				memResource = resource.MustParse(memStr)
			}

			_, err := clients.KubeClient.Create(&v1.Pod{
				ObjectMeta: v12.ObjectMeta{
					Name:                       podReservationName,
					Namespace:                  tags["namespace"],
					Labels:  map[string]string{
						"applicationId": app.applicationID,
						"queue": app.queue,
					},
					Annotations: map[string]string{
						"yunikorn.apache.org/task-group-name": tgName,
						"yunikorn.apache.org/appID": app.applicationID,
						"yunikorn.apache.org/pod-type": "reservation",
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:                     "fake-tg-pod",
							Image:                    "k8s.gcr.io/pause",
							//Command:                  []string{"sleep", "3600"},
							Resources:                v1.ResourceRequirements{
								Requests: map[v1.ResourceName]resource.Quantity {
									v1.ResourceCPU : cpuResource,
									v1.ResourceMemory: memResource,
								},
							},
						},
					},
					RestartPolicy: "Never",
					SchedulerName: "yunikorn",
				},
			})
			if err != nil {
				log.Logger().Error("failed to create reservation pod", zap.Error(err))
				continue
			}
		}

	}
	return app
}

func NewApplication(appID, queueName, user string, tags map[string]string, scheduler api.SchedulerAPI) *Application {
	taskMap := make(map[string]*Task)
	app := &Application{
		applicationID: appID,
		queue:         queueName,
		partition:     constants.DefaultPartition,
		user:          user,
		taskMap:       taskMap,
		tags:          tags,
		lock:          &sync.RWMutex{},
		schedulerAPI:  scheduler,
	}

	var states = events.States().Application
	app.sm = fsm.NewFSM(
		states.New,
		fsm.Events{
			{Name: string(events.SubmitApplication),
				Src: []string{states.New},
				Dst: states.Submitted},
			{Name: string(events.RecoverApplication),
				Src: []string{states.New},
				Dst: states.Recovering},
			{Name: string(events.AcceptApplication),
				Src: []string{states.Submitted, states.Recovering},
				Dst: states.Accepted},
			{Name: string(events.TryReserve),
				Src: []string{states.Accepted},
				Dst: states.Reserving},
			{Name: string(events.RunApplication),
				Src: []string{states.Accepted, states.Reserving, states.Running},
				Dst: states.Running},
			{Name: string(events.CompleteApplication),
				Src: []string{states.Running},
				Dst: states.Completed},
			{Name: string(events.RejectApplication),
				Src: []string{states.Submitted},
				Dst: states.Rejected},
			{Name: string(events.FailApplication),
				Src: []string{states.Submitted, states.Rejected, states.Accepted, states.Running},
				Dst: states.Failed},
			{Name: string(events.KillApplication),
				Src: []string{states.Accepted, states.Running},
				Dst: states.Killing},
			{Name: string(events.KilledApplication),
				Src: []string{states.Killing},
				Dst: states.Killed},
		},
		fsm.Callbacks{
			string(events.SubmitApplication):   app.handleSubmitApplicationEvent,
			string(events.RecoverApplication):  app.handleRecoverApplicationEvent,
			string(events.RejectApplication):   app.handleRejectApplicationEvent,
			string(events.CompleteApplication): app.handleCompleteApplicationEvent,
			events.EnterState:                  app.enterState,
		},
	)

	return app
}

func (app *Application) handle(ev events.ApplicationEvent) error {
	// Locking mechanism:
	// 1) when handle event transitions, we first obtain the object's lock,
	//    this helps us to place a pre-check before entering here, in case
	//    we receive some invalidate events. If this introduces performance
	//    regression, a possible way to optimize is to use a separate lock
	//    to protect the transition phase.
	// 2) Note, state machine calls those callbacks here, we must ensure
	//    they are lock-free calls. Otherwise the callback will be blocked
	//    because the lock is already held here.
	app.lock.Lock()
	defer app.lock.Unlock()
	err := app.sm.Event(string(ev.GetEvent()), ev.GetArgs()...)
	// handle the same state transition not nil error (limit of fsm).
	if err != nil && err.Error() != "no transition" {
		return err
	}
	return nil
}

func (app *Application) canHandle(ev events.ApplicationEvent) bool {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.sm.Can(string(ev.GetEvent()))
}

func (app *Application) hasTaskGroups() bool {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return len(app.taskGroups) > 0
}

func (app *Application) GetTask(taskID string) (interfaces.ManagedTask, error) {
	app.lock.RLock()
	defer app.lock.RUnlock()
	if task, ok := app.taskMap[taskID]; ok {
		return task, nil
	}
	return nil, fmt.Errorf("task %s doesn't exist in application %s",
		taskID, app.applicationID)
}

func (app *Application) GetApplicationID() string {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.applicationID
}

func (app *Application) GetQueue() string {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.queue
}

func (app *Application) GetUser() string {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.user
}

func (app *Application) addTask(task *Task) {
	app.lock.Lock()
	defer app.lock.Unlock()
	if _, ok := app.taskMap[task.taskID]; ok {
		// skip adding duplicate task
		return
	}
	app.taskMap[task.taskID] = task
}

func (app *Application) removeTask(taskID string) error {
	app.lock.Lock()
	defer app.lock.Unlock()
	if _, ok := app.taskMap[taskID]; ok {
		delete(app.taskMap, taskID)
		log.Logger().Info("task removed",
			zap.String("appID", app.applicationID),
			zap.String("taskID", taskID))
		return nil
	}
	return fmt.Errorf("task %s is not found in application %s",
		taskID, app.applicationID)
}

func (app *Application) GetApplicationState() string {
	return app.sm.Current()
}

func (app *Application) GetPendingTasks() []*Task {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.getTasks(events.States().Task.Pending)
}

func (app *Application) GetNewTasks() []*Task {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.getTasks(events.States().Task.New)
}

func (app *Application) GetAllocatedTasks() []*Task {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.getTasks(events.States().Task.Allocated)
}

func (app *Application) getTasks(state string) []*Task {
	taskList := make([]*Task, 0)
	if len(app.taskMap) > 0 {
		for _, task := range app.taskMap {
			if task.GetTaskState() == state {
				taskList = append(taskList, task)
			}
		}
	}

	// sort the task based on creation time
	sort.Slice(taskList, func(i, j int) bool {
		l := taskList[i]
		r := taskList[j]
		return l.createTime.Before(r.createTime)
	})

	return taskList
}

func (app *Application) GetTags() map[string]string {
	return app.tags
}

func (app *Application) getNonTerminatedTaskAlias() []string {
	var nonTerminatedTaskAlias []string
	for _, task := range app.taskMap {
		if !task.isTerminated() {
			nonTerminatedTaskAlias = append(nonTerminatedTaskAlias, task.alias)
		}
	}
	return nonTerminatedTaskAlias
}

// only for testing
// this is just used for testing, it is not supposed to change state like this
func (app *Application) SetState(state string) {
	app.lock.Lock()
	defer app.lock.Unlock()
	app.sm.SetState(state)
}

// This is called in every scheduling interval,
// we are not using dispatcher here because we want to
// make state transition in sync mode in order to prevent
// generating too many duplicate events. However, it must
// ensure non of these calls is expensive, usually, they
// do nothing more than just triggering the state transition.
func (app *Application) Schedule() {
	var states = events.States().Application
	switch app.GetApplicationState() {
	case states.New:
		ev := NewSubmitApplicationEvent(app.GetApplicationID())
		if err := app.handle(ev); err != nil {
			log.Logger().Warn("failed to handle SUBMIT app event",
				zap.Error(err))
		}
	case states.Accepted:
		if app.hasTaskGroups() {
			ev := NewSimpleApplicationEvent(app.applicationID, events.TryReserve)
			if err := app.handle(ev); err != nil {
				log.Logger().Warn("failed to handle TryReserve app event",
					zap.Error(err))
			}
		} else {
			ev := NewRunApplicationEvent(app.GetApplicationID())
			if err := app.handle(ev); err != nil {
				log.Logger().Warn("failed to handle RUN app event",
					zap.Error(err))
			}
		}
	case states.Reserving:
		log.Logger().Info("try reserving", zap.String("app", app.applicationID))
		for _, task := range app.GetNewTasks() {
			if task.taskType == reservation {
				// for each new task, we do a sanity check before moving the state to Pending_Schedule
				if err := task.sanityCheckBeforeScheduling(); err == nil {
					// note, if we directly trigger submit task event, it may spawn too many duplicate
					// events, because a task might be submitted multiple times before its state transits to PENDING.
					if handleErr := task.handle(
						NewSimpleTaskEvent(task.applicationID, task.taskID, events.InitTask)); handleErr != nil {
						// something goes wrong when transit task to PENDING state,
						// this should not happen because we already checked the state
						// before calling the transition. Nowhere to go, just log the error.
						log.Logger().Warn("init task failed", zap.Error(err))
					}
				} else {
					events.GetRecorder().Event(task.GetTaskPod(), v1.EventTypeWarning, "FailedScheduling", err.Error())
					log.Logger().Debug("task is not ready for scheduling",
						zap.String("appID", task.applicationID),
						zap.String("taskID", task.taskID),
						zap.Error(err))
				}
			}
		}

		// TODO fix me
		// simple check, all reservations are made, move to next stage
		desire := int32(0)
		for _, tg := range app.taskGroups {
			desire += tg.MinMember
		}
		actual := len(app.getTasks(events.States().Task.Bound))
		log.Logger().Info("reservation status",
			zap.Int32("desire", desire),
			zap.Int("actual", actual))
		if desire == int32(actual) {
			ev := NewRunApplicationEvent(app.GetApplicationID())
			if err := app.handle(ev); err != nil {
				log.Logger().Warn("failed to handle RUN app event",
					zap.Error(err))
			}
		}
	case states.Running:
		if len(app.GetNewTasks()) > 0 {
			for _, task := range app.GetNewTasks() {
				// for each new task, we do a sanity check before moving the state to Pending_Schedule
				if err := task.sanityCheckBeforeScheduling(); err == nil {
					// note, if we directly trigger submit task event, it may spawn too many duplicate
					// events, because a task might be submitted multiple times before its state transits to PENDING.
					if handleErr := task.handle(
						NewSimpleTaskEvent(task.applicationID, task.taskID, events.InitTask)); handleErr != nil {
						// something goes wrong when transit task to PENDING state,
						// this should not happen because we already checked the state
						// before calling the transition. Nowhere to go, just log the error.
						log.Logger().Warn("init task failed", zap.Error(err))
					}
				} else {
					events.GetRecorder().Event(task.GetTaskPod(), v1.EventTypeWarning, "FailedScheduling", err.Error())
					log.Logger().Debug("task is not ready for scheduling",
						zap.String("appID", task.applicationID),
						zap.String("taskID", task.taskID),
						zap.Error(err))
				}
			}
		}
	default:
		log.Logger().Debug("skipping scheduling application",
			zap.String("appID", app.GetApplicationID()),
			zap.String("appState", app.GetApplicationState()))
	}
}

func (app *Application) handleSubmitApplicationEvent(event *fsm.Event) {
	log.Logger().Info("handle app submission",
		zap.String("app", app.String()),
		zap.String("clusterID", conf.GetSchedulerConf().ClusterID))
	err := app.schedulerAPI.Update(
		&si.UpdateRequest{
			NewApplications: []*si.AddApplicationRequest{
				{
					ApplicationID: app.applicationID,
					QueueName:     app.queue,
					PartitionName: app.partition,
					Ugi: &si.UserGroupInformation{
						User: app.user,
					},
					Tags: app.tags,
				},
			},
			RmID: conf.GetSchedulerConf().ClusterID,
		})

	if err != nil {
		// submission failed
		log.Logger().Warn("failed to submit app", zap.Error(err))
		dispatcher.Dispatch(NewFailApplicationEvent(app.applicationID))
	}
}

func (app *Application) handleRecoverApplicationEvent(event *fsm.Event) {
	log.Logger().Info("handle app recovering",
		zap.String("app", app.String()),
		zap.String("clusterID", conf.GetSchedulerConf().ClusterID))
	err := app.schedulerAPI.Update(
		&si.UpdateRequest{
			NewApplications: []*si.AddApplicationRequest{
				{
					ApplicationID: app.applicationID,
					QueueName:     app.queue,
					PartitionName: app.partition,
					Ugi: &si.UserGroupInformation{
						User: app.user,
					},
					Tags: app.tags,
				},
			},
			RmID: conf.GetSchedulerConf().ClusterID,
		})

	if err != nil {
		// submission failed
		log.Logger().Warn("failed to submit app", zap.Error(err))
		dispatcher.Dispatch(NewFailApplicationEvent(app.applicationID))
	}
}

func (app *Application) handleRejectApplicationEvent(event *fsm.Event) {
	log.Logger().Info("app is rejected by scheduler", zap.String("appID", app.applicationID))
	// for rejected apps, we directly move them to failed state
	dispatcher.Dispatch(NewFailApplicationEvent(app.applicationID))
}

func (app *Application) handleCompleteApplicationEvent(event *fsm.Event) {
	// TODO app lifecycle updates
}

func (app *Application) enterState(event *fsm.Event) {
	log.Logger().Debug("shim app state transition",
		zap.String("app", app.applicationID),
		zap.String("source", event.Src),
		zap.String("destination", event.Dst),
		zap.String("event", event.Event))
}
