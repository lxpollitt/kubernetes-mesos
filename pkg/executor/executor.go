package executor

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/dockertools"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/watch"
	"github.com/fsouza/go-dockerclient"
	"github.com/gogo/protobuf/proto"
	log "github.com/golang/glog"
	bindings "github.com/mesos/mesos-go/executor"
	mesos "github.com/mesos/mesos-go/mesosproto"
	mutil "github.com/mesos/mesos-go/mesosutil"
	"github.com/mesosphere/kubernetes-mesos/pkg/executor/messages"
	"github.com/mesosphere/kubernetes-mesos/pkg/scheduler/meta"
	"gopkg.in/v2/yaml"
)

const (
	containerPollTime = 300 * time.Millisecond
	launchGracePeriod = 5 * time.Minute
)

type stateType int32

const (
	disconnectedState stateType = iota
	connectedState
	doneState
)

type kuberTask struct {
	mesosTaskInfo *mesos.TaskInfo
	podName       string
}

// KubernetesExecutor is an mesos executor that runs pods
// in a minion machine.
type KubernetesExecutor struct {
	kl           *kubelet.Kubelet // the kubelet instance.
	updateChan   chan<- interface{}
	state        stateType
	tasks        map[string]*kuberTask
	pods         map[string]*api.BoundPod
	lock         sync.RWMutex
	sourcename   string
	client       *client.Client
	events       <-chan watch.Event
	done         chan struct{} // signals shutdown
	outgoing     chan func() (mesos.Status, error)
	dockerClient dockertools.DockerInterface
}

func (k *KubernetesExecutor) getState() stateType {
	return stateType(atomic.LoadInt32((*int32)(&k.state)))
}

func (k *KubernetesExecutor) isConnected() bool {
	return connectedState == k.getState()
}

func (k *KubernetesExecutor) swapState(from, to stateType) bool {
	return atomic.CompareAndSwapInt32((*int32)(&k.state), int32(from), int32(to))
}

// New creates a new kubernetes executor.
func New(kl *kubelet.Kubelet, ch chan<- interface{}, ns string, cl *client.Client, w watch.Interface, dc dockertools.DockerInterface) *KubernetesExecutor {
	//TODO(jdef) do something real with these events..
	events := w.ResultChan()
	if events != nil {
		go func() {
			for e := range events {
				// e ~= watch.Event { ADDED, *api.Event }
				log.V(1).Info(e)
			}
		}()
	}
	k := &KubernetesExecutor{
		kl:           kl,
		updateChan:   ch,
		state:        disconnectedState,
		tasks:        make(map[string]*kuberTask),
		pods:         make(map[string]*api.BoundPod),
		sourcename:   ns,
		client:       cl,
		events:       events,
		done:         make(chan struct{}),
		outgoing:     make(chan func() (mesos.Status, error), 1024),
		dockerClient: dc,
	}
	go k.sendLoop()
	return k
}

func (k *KubernetesExecutor) isDone() bool {
	select {
	case <-k.done:
		return true
	default:
		return false
	}
}

// Registered is called when the executor is successfully registered with the slave.
func (k *KubernetesExecutor) Registered(driver bindings.ExecutorDriver,
	executorInfo *mesos.ExecutorInfo, frameworkInfo *mesos.FrameworkInfo, slaveInfo *mesos.SlaveInfo) {
	if k.isDone() {
		return
	}
	log.Infof("Executor %v of framework %v registered with slave %v\n",
		executorInfo, frameworkInfo, slaveInfo)
	if !k.swapState(disconnectedState, connectedState) {
		//programming error?
		panic("already connected?!")
	}
}

// Reregistered is called when the executor is successfully re-registered with the slave.
// This can happen when the slave fails over.
func (k *KubernetesExecutor) Reregistered(driver bindings.ExecutorDriver, slaveInfo *mesos.SlaveInfo) {
	if k.isDone() {
		return
	}
	log.Infof("Reregistered with slave %v\n", slaveInfo)
	if !k.swapState(disconnectedState, connectedState) {
		//programming error?
		panic("already connected?!")
	}
}

// Disconnected is called when the executor is disconnected with the slave.
func (k *KubernetesExecutor) Disconnected(driver bindings.ExecutorDriver) {
	if k.isDone() {
		return
	}
	log.Infof("Slave is disconnected\n")
	if !k.swapState(connectedState, disconnectedState) {
		//programming error?
		panic("already disconnected?!")
	}
}

// LaunchTask is called when the executor receives a request to launch a task.
func (k *KubernetesExecutor) LaunchTask(driver bindings.ExecutorDriver, taskInfo *mesos.TaskInfo) {
	if k.isDone() {
		return
	}
	log.Infof("Launch task %v\n", taskInfo)

	if !k.isConnected() {
		log.Warningf("Ignore launch task because the executor is disconnected\n")
		k.sendStatus(driver, newStatus(taskInfo.GetTaskId(), mesos.TaskState_TASK_FAILED,
			messages.ExecutorUnregistered))
		return
	}

	var pod api.BoundPod
	if err := yaml.Unmarshal(taskInfo.GetData(), &pod); err != nil {
		log.Warningf("Failed to extract yaml data from the taskInfo.data %v\n", err)
		k.sendStatus(driver, newStatus(taskInfo.GetTaskId(), mesos.TaskState_TASK_FAILED,
			messages.UnmarshalTaskDataFailure))
		return
	}

	k.lock.Lock()
	defer k.lock.Unlock()

	taskId := taskInfo.GetTaskId().GetValue()
	if _, found := k.tasks[taskId]; found {
		log.Warningf("task already launched\n")
		// Not to send back TASK_RUNNING here, because
		// may be duplicated messages or duplicated task id.
		return
	}
	// remember this task so that:
	// (a) we ignore future launches for it
	// (b) we have a record of it so that we can kill it if needed
	// (c) we're leaving podName == "" for now, indicates we don't need to delete containers
	k.tasks[taskId] = &kuberTask{
		mesosTaskInfo: taskInfo,
	}
	go k.launchTask(driver, taskId, &pod)
}

func (k *KubernetesExecutor) getPidInfo(name string) (api.PodStatus, error) {
	return k.kl.GetPodStatus(name, "")
}

// async continuation of LaunchTask
func (k *KubernetesExecutor) launchTask(driver bindings.ExecutorDriver, taskId string, pod *api.BoundPod) {

	//HACK(jdef): cloned binding construction from k8s plugin/pkg/scheduler/scheduler.go
	binding := &api.Binding{
		ObjectMeta: api.ObjectMeta{
			Namespace:   pod.Namespace,
			Annotations: make(map[string]string),
		},
		PodID: pod.Name,
		Host:  pod.Annotations[meta.BindingHostKey],
	}

	// forward the bindings that the scheduler wants to apply
	for k, v := range pod.Annotations {
		binding.Annotations[k] = v
	}

	log.Infof("Binding '%v' to '%v' with annotations %+v...", binding.PodID, binding.Host, binding.Annotations)
	ctx := api.WithNamespace(api.NewDefaultContext(), binding.Namespace)
	err := k.client.Post().Namespace(api.NamespaceValue(ctx)).Resource("bindings").Body(binding).Do().Error()
	if err != nil {
		k.sendStatus(driver, newStatus(mutil.NewTaskID(taskId), mesos.TaskState_TASK_FAILED,
			messages.CreateBindingFailure))
		return
	}

	podFullName := kubelet.GetPodFullName(&api.BoundPod{
		ObjectMeta: api.ObjectMeta{
			Name:        pod.Name,
			Namespace:   pod.Namespace,
			Annotations: map[string]string{kubelet.ConfigSourceAnnotationKey: k.sourcename},
		},
	})

	// allow a recently failed-over scheduler the chance to recover the task/pod binding:
	// it may have failed and recovered before the apiserver is able to report the updated
	// binding information. replays of this status event will signal to the scheduler that
	// the apiserver should be up-to-date.
	data, err := json.Marshal(api.PodStatusResult{
		ObjectMeta: api.ObjectMeta{
			Name:     podFullName,
			SelfLink: "/podstatusresult",
		},
	})
	if err != nil {
		log.Errorf("failed to marshal pod status result: %v", err)
		k.sendStatus(driver, newStatus(mutil.NewTaskID(taskId), mesos.TaskState_TASK_FAILED, err.Error()))
		return
	}

	k.lock.Lock()
	defer k.lock.Unlock()

	// Add the task.
	task, found := k.tasks[taskId]
	if !found {
		log.V(1).Infof("task %v no longer on record, probably killed, aborting launch sequence - reporting lost", taskId)
		k.reportLostTask(driver, taskId, messages.LaunchTaskFailed)
		return
	}

	// from here on, we need to delete containers associated with the task
	// upon it going into a terminal state
	task.podName = podFullName
	k.pods[podFullName] = pod

	// Send the pod updates to the channel.
	update := kubelet.PodUpdate{Op: kubelet.SET}
	for _, p := range k.pods {
		update.Pods = append(update.Pods, *p)
	}
	k.updateChan <- update

	statusUpdate := &mesos.TaskStatus{
		TaskId:  mutil.NewTaskID(taskId),
		State:   mesos.TaskState_TASK_STARTING.Enum(),
		Message: proto.String(messages.CreateBindingSuccess),
		Data:    data,
	}
	k.sendStatus(driver, statusUpdate)

	// Delay reporting 'task running' until container is up.
	go k._launchTask(driver, taskId, podFullName)
}

func (k *KubernetesExecutor) _launchTask(driver bindings.ExecutorDriver, taskId, podFullName string) {

	expired := make(chan struct{})
	time.AfterFunc(launchGracePeriod, func() { close(expired) })

	getMarshalledInfo := func() (data []byte, cancel bool) {
		// potentially long call..
		if podStatus, err := k.getPidInfo(podFullName); err == nil {
			select {
			case <-expired:
				cancel = true
			default:
				k.lock.Lock()
				defer k.lock.Unlock()
				if _, found := k.tasks[taskId]; !found {
					// don't bother with the pod status if the task is already gone
					cancel = true
					break
				} else if podStatus.Phase != api.PodRunning {
					// avoid sending back a running status before it's really running
					break
				}
				log.V(2).Infof("Found pod status: '%v'", podStatus)
				result := api.PodStatusResult{
					ObjectMeta: api.ObjectMeta{
						Name:     podFullName,
						SelfLink: "/podstatusresult",
					},
					Status: podStatus,
				}
				if data, err = json.Marshal(result); err != nil {
					log.Errorf("failed to marshal pod status result: %v", err)
				}
			}
		}
		return
	}

waitForRunningPod:
	for {
		select {
		case <-expired:
			log.Warningf("Launch expired grace period of '%v'", launchGracePeriod)
			break waitForRunningPod
		case <-time.After(containerPollTime):
			if data, cancel := getMarshalledInfo(); cancel {
				break waitForRunningPod
			} else if data == nil {
				continue waitForRunningPod
			} else {
				k.lock.Lock()
				defer k.lock.Unlock()
				if _, found := k.tasks[taskId]; !found {
					goto reportLost
				}

				statusUpdate := &mesos.TaskStatus{
					TaskId:  mutil.NewTaskID(taskId),
					State:   mesos.TaskState_TASK_RUNNING.Enum(),
					Message: proto.String(fmt.Sprintf("pod-running:%s", podFullName)),
					Data:    data,
				}

				k.sendStatus(driver, statusUpdate)

				// continue to monitor the health of the pod
				go k.__launchTask(driver, taskId, podFullName)
				return
			}
		}
	}

	k.lock.Lock()
	defer k.lock.Unlock()
reportLost:
	k.reportLostTask(driver, taskId, messages.LaunchTaskFailed)
}

func (k *KubernetesExecutor) __launchTask(driver bindings.ExecutorDriver, taskId, podFullName string) {
	// TODO(nnielsen): Monitor health of pod and report if lost.
	// Should we also allow this to fail a couple of times before reporting lost?
	// What if the docker daemon is restarting and we can't connect, but it's
	// going to bring the pods back online as soon as it restarts?
	knownPod := func() bool {
		_, err := k.getPidInfo(podFullName)
		return err == nil
	}
	// Wait for the pod to go away and stop monitoring once it does
	// TODO (jdefelice) replace with an /events watch?
	for {
		time.Sleep(containerPollTime)
		if k.checkForLostPodTask(driver, taskId, knownPod) {
			return
		}
	}
}

// Intended to be executed as part of the pod monitoring loop, this fn (ultimately) checks with Docker
// whether the pod is running. It will only return false if the task is still registered and the pod is
// registered in Docker. Otherwise it returns true. If there's still a task record on file, but no pod
// in Docker, then we'll also send a TASK_LOST event.
func (k *KubernetesExecutor) checkForLostPodTask(driver bindings.ExecutorDriver, taskId string, isKnownPod func() bool) bool {
	// TODO (jdefelice) don't send false alarms for deleted pods (KILLED tasks)
	k.lock.Lock()
	defer k.lock.Unlock()

	// TODO(jdef) we should really consider k.pods here, along with what docker is reporting, since the kubelet
	// may constantly attempt to instantiate a pod as long as it's in the pod state that we're handing to it.
	// otherwise, we're probably reporting a TASK_LOST prematurely. Should probably consult RestartPolicy to
	// determine appropriate behavior. Should probably also gracefully handle docker daemon restarts.
	if _, ok := k.tasks[taskId]; ok {
		if isKnownPod() {
			return false
		} else {
			log.Warningf("Detected lost pod, reporting lost task %v", taskId)
			k.reportLostTask(driver, taskId, messages.ContainersDisappeared)
		}
	} else {
		log.V(2).Infof("Task %v no longer registered, stop monitoring for lost pods", taskId)
	}
	return true
}

// KillTask is called when the executor receives a request to kill a task.
func (k *KubernetesExecutor) KillTask(driver bindings.ExecutorDriver, taskId *mesos.TaskID) {
	if k.isDone() {
		return
	}
	log.Infof("Kill task %v\n", taskId)

	if !k.isConnected() {
		//TODO(jdefelice) sent TASK_LOST here?
		log.Warningf("Ignore kill task because the executor is disconnected\n")
		return
	}

	k.lock.Lock()
	defer k.lock.Unlock()
	k.killPodForTask(driver, taskId.GetValue(), messages.TaskKilled)
}

// Kills the pod associated with the given task. Assumes that the caller is locking around
// pod and task storage.
func (k *KubernetesExecutor) killPodForTask(driver bindings.ExecutorDriver, tid, reason string) {
	k.removePodTask(driver, tid, reason, mesos.TaskState_TASK_KILLED)
}

// Reports a lost task to the slave and updates internal task and pod tracking state.
// Assumes that the caller is locking around pod and task state.
func (k *KubernetesExecutor) reportLostTask(driver bindings.ExecutorDriver, tid, reason string) {
	k.removePodTask(driver, tid, reason, mesos.TaskState_TASK_LOST)
}

// returns a chan that closes when the pod is no longer running in Docker
func (k *KubernetesExecutor) removePodTask(driver bindings.ExecutorDriver, tid, reason string, state mesos.TaskState) {
	task, ok := k.tasks[tid]
	if !ok {
		log.V(1).Infof("Failed to remove task, unknown task %v\n", tid)
		return
	}
	delete(k.tasks, tid)

	pid := task.podName
	if _, found := k.pods[pid]; !found {
		log.Warningf("Cannot remove unknown pod %v for task %v", pid, tid)
	} else {
		log.V(2).Infof("deleting pod %v for task %v", pid, tid)
		delete(k.pods, pid)

		// Send the pod updates to the channel.
		update := kubelet.PodUpdate{Op: kubelet.SET}
		for _, p := range k.pods {
			update.Pods = append(update.Pods, *p)
		}
		k.updateChan <- update
	}
	// TODO(jdef): ensure that the update propagates, perhaps return a signal chan?
	k.sendStatus(driver, newStatus(mutil.NewTaskID(tid), state, reason))
}

// FrameworkMessage is called when the framework sends some message to the executor
func (k *KubernetesExecutor) FrameworkMessage(driver bindings.ExecutorDriver, message string) {
	if k.isDone() {
		return
	}
	if !k.isConnected() {
		log.Warningf("Ignore framework message because the executor is disconnected\n")
		return
	}

	log.Infof("Receives message from framework %v\n", message)
	//TODO(jdef) master reported a lost task, reconcile this! @see scheduler.go:handleTaskLost
	if strings.HasPrefix("task-lost:", message) && len(message) > 10 {
		taskId := message[10:]
		if taskId != "" {
			// clean up pod state
			k.reportLostTask(driver, taskId, messages.TaskLostAck)
		}
	}
}

// Shutdown is called when the executor receives a shutdown request.
func (k *KubernetesExecutor) Shutdown(driver bindings.ExecutorDriver) {
	if k.isDone() {
		return
	}
	close(k.done)

	log.Infoln("Shutdown the executor")
	defer func() {
		for !k.swapState(k.getState(), doneState) {
		}
	}()

	func() {
		k.lock.Lock()
		defer k.lock.Unlock()
		k.tasks = map[string]*kuberTask{}
	}()

	// according to docs, mesos will generate TASK_LOST updates for us
	// if needed, so don't take extra time to do that here.

	// also, clear the pod configuration so that after we issue our Kill
	// kubernetes doesn't start spinning things up before we exit.
	k.updateChan <- kubelet.PodUpdate{Op: kubelet.SET}

	KillKubeletContainers(k.dockerClient)
}

// Destroy existing k8s containers
func KillKubeletContainers(dockerClient dockertools.DockerInterface) {
	if containers, err := dockertools.GetKubeletDockerContainers(dockerClient, true); err == nil {
		opts := docker.RemoveContainerOptions{
			RemoveVolumes: true,
			Force:         true,
		}
		for _, container := range containers {
			opts.ID = container.ID
			log.V(2).Infof("Removing container: %v", opts.ID)
			if err := dockerClient.RemoveContainer(opts); err != nil {
				log.Warning(err)
			}
		}
	} else {
		log.Warningf("Failed to list kubelet docker containers: %v", err)
	}
}

// Error is called when some error happens.
func (k *KubernetesExecutor) Error(driver bindings.ExecutorDriver, message string) {
	log.Errorln(message)
}

func newStatus(taskId *mesos.TaskID, state mesos.TaskState, message string) *mesos.TaskStatus {
	return &mesos.TaskStatus{
		TaskId:  taskId,
		State:   &state,
		Message: proto.String(message),
	}
}

func (k *KubernetesExecutor) sendStatus(driver bindings.ExecutorDriver, status *mesos.TaskStatus) {
	select {
	case <-k.done:
	default:
		k.outgoing <- func() (mesos.Status, error) { return driver.SendStatusUpdate(status) }
	}
}

func (k *KubernetesExecutor) sendFrameworkMessage(driver bindings.ExecutorDriver, msg string) {
	select {
	case <-k.done:
	default:
		k.outgoing <- func() (mesos.Status, error) { return driver.SendFrameworkMessage(msg) }
	}
}

func (k *KubernetesExecutor) sendLoop() {
	defer log.V(1).Info("sender loop exiting")
	for {
		select {
		case <-k.done:
			return
		default:
			if !k.isConnected() {
				select {
				case <-k.done:
				case <-time.After(1 * time.Second):
				}
				continue
			}
			sender, ok := <-k.outgoing
			if !ok {
				// programming error
				panic("someone closed the outgoing channel")
			}
			if status, err := sender(); err == nil {
				continue
			} else {
				log.Error(err)
				if status == mesos.Status_DRIVER_ABORTED {
					return
				}
			}
			// attempt to re-queue the sender
			select {
			case <-k.done:
			case k.outgoing <- sender:
			}
		}
	}
}
