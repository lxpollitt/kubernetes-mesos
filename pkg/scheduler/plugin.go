package scheduler

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/errors"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/cache"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/envvars"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	algorithm "github.com/GoogleCloudPlatform/kubernetes/pkg/scheduler"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/watch"
	plugin "github.com/GoogleCloudPlatform/kubernetes/plugin/pkg/scheduler"
	log "github.com/golang/glog"
	mesos "github.com/mesos/mesos-go/mesosproto"
	mutil "github.com/mesos/mesos-go/mesosutil"
	"github.com/mesosphere/kubernetes-mesos/pkg/offers"
	"github.com/mesosphere/kubernetes-mesos/pkg/queue"
	annotation "github.com/mesosphere/kubernetes-mesos/pkg/scheduler/meta"
	"github.com/mesosphere/kubernetes-mesos/pkg/scheduler/podtask"
	"gopkg.in/v2/yaml"
)

const (
	enqueuePopTimeout  = 200 * time.Millisecond
	enqueueWaitTimeout = 1 * time.Second
	yieldPopTimeout    = 200 * time.Millisecond
	yieldWaitTimeout   = 1 * time.Second
)

// scheduler abstraction to allow for easier unit testing
type SchedulerInterface interface {
	sync.Locker
	RLocker() sync.Locker
	SlaveIndex

	algorithm() PodScheduleFunc
	createPodTask(api.Context, *api.Pod) (*podtask.T, error)
	getTask(taskId string) (task *podtask.T, currentState podtask.StateType)
	offers() offers.Registry
	registerPodTask(*podtask.T, error) (*podtask.T, error)
	taskForPod(podID string) (taskID string, ok bool)
	unregisterPodTask(*podtask.T)
	killTask(taskId string) error
	launchTask(*podtask.T) error
}

type k8smScheduler struct {
	*KubernetesScheduler
}

func (k *k8smScheduler) algorithm() PodScheduleFunc {
	return k.KubernetesScheduler.scheduleFunc
}

func (k *k8smScheduler) offers() offers.Registry {
	return k.KubernetesScheduler.offers
}

func (k *k8smScheduler) taskForPod(podID string) (string, bool) {
	return k.KubernetesScheduler.taskRegistry.TaskForPod(podID)
}

func (k *k8smScheduler) createPodTask(ctx api.Context, pod *api.Pod) (*podtask.T, error) {
	return podtask.New(ctx, pod, k.executor)
}

func (k *k8smScheduler) getTask(taskId string) (*podtask.T, podtask.StateType) {
	return k.KubernetesScheduler.taskRegistry.Get(taskId)
}

func (k *k8smScheduler) registerPodTask(task *podtask.T, err error) (*podtask.T, error) {
	return k.KubernetesScheduler.taskRegistry.Register(task, err)
}

func (k *k8smScheduler) slaveFor(id string) (slave *Slave, ok bool) {
	slave, ok = k.slaves[id]
	return
}

func (k *k8smScheduler) unregisterPodTask(task *podtask.T) {
	k.KubernetesScheduler.taskRegistry.Unregister(task)
}

func (k *k8smScheduler) killTask(taskId string) error {
	// assume caller is holding scheduler lock
	killTaskId := mutil.NewTaskID(taskId)
	_, err := k.KubernetesScheduler.driver.KillTask(killTaskId)
	return err
}

func (k *k8smScheduler) launchTask(task *podtask.T) error {
	// assume caller is holding scheduler lock
	taskList := []*mesos.TaskInfo{task.TaskInfo}
	offerIds := []*mesos.OfferID{task.Offer.Details().Id}
	filters := &mesos.Filters{}
	_, err := k.KubernetesScheduler.driver.LaunchTasks(offerIds, taskList, filters)
	return err
}

type binder struct {
	api    SchedulerInterface
	client *client.Client
}

// implements binding.Registry, launches the pod-associated-task in mesos
func (b *binder) Bind(binding *api.Binding) error {

	ctx := api.WithNamespace(api.NewContext(), binding.Namespace)

	// default upstream scheduler passes pod.Name as binding.PodID
	podKey, err := podtask.MakePodKey(ctx, binding.PodID)
	if err != nil {
		return err
	}

	b.api.Lock()
	defer b.api.Unlock()

	taskId, exists := b.api.taskForPod(podKey)
	if !exists {
		log.Infof("Could not resolve pod %s to task id", podKey)
		return noSuchPodErr
	}

	switch task, state := b.api.getTask(taskId); state {
	case podtask.StatePending:
		return b.bind(ctx, binding, task)
	default:
		// in this case it's likely that the pod has been deleted between Schedule
		// and Bind calls
		log.Infof("No pending task for pod %s", podKey)
		return noSuchPodErr
	}
}

// assumes that: caller has acquired scheduler lock and that the task is still pending
func (b *binder) bind(ctx api.Context, binding *api.Binding, task *podtask.T) (err error) {
	// sanity check: ensure that the task hasAcceptedOffer(), it's possible that between
	// Schedule() and now that the offer for this task was rescinded or invalidated.
	// ((we should never see this here))
	if !task.HasAcceptedOffer() {
		return fmt.Errorf("task has not accepted a valid offer %v", task.ID)
	}

	// By this time, there is a chance that the slave is disconnected.
	offerId := task.GetOfferId()
	if offer, ok := b.api.offers().Get(offerId); !ok || offer.HasExpired() {
		// already rescinded or timed out or otherwise invalidated
		task.Offer.Release()
		task.ClearTaskInfo()
		return fmt.Errorf("failed prior to launchTask due to expired offer for task %v", task.ID)
	}

	if err = b.prepareTaskForLaunch(ctx, binding.Host, task, offerId); err == nil {
		log.V(2).Infof("launching task : %v", task)
		if err = b.api.launchTask(task); err == nil {
			b.api.offers().Invalidate(offerId)
			task.Pod.Status.Host = binding.Host
			task.Set(podtask.Launched)
			return
		}
	}
	task.Offer.Release()
	task.ClearTaskInfo()
	return fmt.Errorf("Failed to launch task %v: %v", task.ID, err)
}

func (b *binder) prepareTaskForLaunch(ctx api.Context, machine string, task *podtask.T, offerId string) error {
	pod, err := b.client.Pods(api.NamespaceValue(ctx)).Get(task.Pod.Name)
	if err != nil {
		return err
	}

	//HACK(jdef): adapted from https://github.com/GoogleCloudPlatform/kubernetes/blob/release-0.6/pkg/registry/pod/bound_pod_factory.go
	envVars, err := b.getServiceEnvironmentVariables(ctx)
	if err != nil {
		return err
	}

	// as of k8s release-0.8 the conversion from api.Pod to api.BoundPod preserves the following
	// - Name (same as api.Binding.PodID)
	// - Namespace
	// - UID
	boundPod := &api.BoundPod{}
	if err := api.Scheme.Convert(pod, boundPod); err != nil {
		return err
	}
	for ix, container := range boundPod.Spec.Containers {
		boundPod.Spec.Containers[ix].Env = append(container.Env, envVars...)
	}

	// Make a dummy self link so that references to this bound pod will work.
	boundPod.SelfLink = "/api/v1beta1/boundPods/" + boundPod.Name

	if boundPod.Annotations == nil {
		boundPod.Annotations = make(map[string]string)
	}
	boundPod.Annotations[annotation.BindingHostKey] = machine
	task.SaveRecoveryInfo(boundPod.Annotations)

	//TODO(jdef) using anything other than the default k8s host-port mapping may
	//confuse k8s pod rectification loops. in the future this point may become
	//moot if the kubelet sync's directly against the apiserver /pods state (and
	//eliminates bound pods all together) - since our version of the kubelet does
	//not use the apiserver or etcd channels, we will be in control of all
	//rectification. And BTW we probably want to store this information, somehow,
	//in the binding annotations.
	for _, entry := range task.Ports {
		port := &(boundPod.Spec.Containers[entry.ContainerIdx].Ports[entry.PortIdx])
		port.HostPort = int(entry.OfferPort)
	}

	// the kubelet-executor uses this boundPod to instantiate the pod
	task.TaskInfo.Data, err = yaml.Marshal(&boundPod)
	if err != nil {
		log.V(2).Infof("Failed to marshal the updated boundPod")
		return err
	}
	return nil
}

// getServiceEnvironmentVariables populates a list of environment variables that are use
// in the container environment to get access to services.
// HACK(jdef): adapted from https://github.com/GoogleCloudPlatform/kubernetes/blob/release-0.6/pkg/registry/pod/bound_pod_factory.go
func (b *binder) getServiceEnvironmentVariables(ctx api.Context) (result []api.EnvVar, err error) {
	var services *api.ServiceList
	if services, err = b.client.Services(api.NamespaceValue(ctx)).List(labels.Everything()); err == nil {
		result = envvars.FromServices(services)
	}
	return
}

type kubeScheduler struct {
	api        SchedulerInterface
	podUpdates queue.FIFO
}

// Schedule implements the Scheduler interface of the Kubernetes.
// It returns the selectedMachine's name and error (if there's any).
func (k *kubeScheduler) Schedule(pod api.Pod, unused algorithm.MinionLister) (string, error) {
	log.Infof("Try to schedule pod %v\n", pod.Name)
	ctx := api.WithNamespace(api.NewDefaultContext(), pod.Namespace)

	// default upstream scheduler passes pod.Name as binding.PodID
	podKey, err := podtask.MakePodKey(ctx, pod.Name)
	if err != nil {
		return "", err
	}

	k.api.Lock()
	defer k.api.Unlock()

	if taskID, ok := k.api.taskForPod(podKey); !ok {
		// There's a bit of a potential race here, a pod could have been yielded() and
		// then before we get *here* it could be deleted.
		// We use meta to index the pod in the store since that's what k8s reflector does.
		podName, err := cache.MetaNamespaceKeyFunc(&pod)
		if err != nil {
			log.Warningf("aborting Schedule, unable to understand pod object %+v", &pod)
			return "", noSuchPodErr
		}
		if deleted := k.podUpdates.Poll(podName, queue.DELETE_EVENT); deleted {
			// avoid scheduling a pod that's been deleted between yieldPod() and Schedule()
			log.Infof("aborting Schedule, pod has been deleted %+v", &pod)
			return "", noSuchPodErr
		}
		return k.doSchedule(k.api.registerPodTask(k.api.createPodTask(ctx, &pod)))
	} else {
		//TODO(jdef) it's possible that the pod state has diverged from what
		//we knew previously, we should probably update the task.Pod state here
		//before proceeding with scheduling

		switch task, state := k.api.getTask(taskID); state {
		case podtask.StatePending:
			if pod.UID != task.Pod.UID {
				// we're dealing with a brand new pod spec here, so the old one must have been
				// deleted -- and so our task store is out of sync w/ respect to reality
				//TODO(jdef) reconcile task
				return "", fmt.Errorf("task %v spec is out of sync with pod %v spec, aborting schedule", taskID, pod.Name)
			} else if task.Has(podtask.Launched) {
				// task has been marked as "launched" but the pod binding creation may have failed in k8s,
				// but we're going to let someone else handle it, probably the mesos task error handler
				return "", fmt.Errorf("task %s has already been launched, aborting schedule", taskID)
			} else {
				return k.doSchedule(task, nil)
			}
		default:
			return "", fmt.Errorf("task %s is not pending, nothing to schedule", taskID)
		}
	}
}

// Call ScheduleFunc and subtract some resources, returning the name of the machine the task is scheduled on
func (k *kubeScheduler) doSchedule(task *podtask.T, err error) (string, error) {
	var offer offers.Perishable
	if err == nil {
		offer, err = k.api.algorithm()(k.api.offers(), k.api, task)
	}
	if err != nil {
		return "", err
	}
	details := offer.Details()
	if details == nil {
		return "", fmt.Errorf("offer already invalid/expired for task %v", task.ID)
	}
	slaveId := details.GetSlaveId().GetValue()
	if slave, ok := k.api.slaveFor(slaveId); !ok {
		// not much sense in Release()ing the offer here since its owner died
		offer.Release()
		k.api.offers().Invalidate(details.Id.GetValue())
		task.ClearTaskInfo()
		return "", fmt.Errorf("Slave disappeared (%v) while scheduling task %v", slaveId, task.ID)
	} else {
		if task.Offer != nil && task.Offer != offer {
			return "", fmt.Errorf("task.offer assignment must be idempotent, task %+v: offer %+v", task, offer)
		}
		task.Offer = offer
		task.FillFromDetails(details)
		return slave.HostName, nil
	}
}

type queuer struct {
	lock            sync.Mutex       // shared by condition variables of this struct
	podUpdates      queue.FIFO       // queue of pod updates to be processed
	podQueue        *queue.DelayFIFO // queue of pods to be scheduled
	deltaCond       sync.Cond        // pod changes are available for processing
	unscheduledCond sync.Cond        // there are unscheduled pods for processing
}

func newQueuer(store queue.FIFO) *queuer {
	q := &queuer{
		podQueue:   queue.NewDelayFIFO(),
		podUpdates: store,
	}
	q.deltaCond.L = &q.lock
	q.unscheduledCond.L = &q.lock
	return q
}

func (q *queuer) installDebugHandlers() {
	http.HandleFunc("/debug/scheduler/podqueue", func(w http.ResponseWriter, r *http.Request) {
		for _, x := range q.podQueue.List() {
			if _, err := io.WriteString(w, fmt.Sprintf("%+v\n", x)); err != nil {
				break
			}
		}
	})
	http.HandleFunc("/debug/scheduler/podstore", func(w http.ResponseWriter, r *http.Request) {
		for _, x := range q.podUpdates.List() {
			if _, err := io.WriteString(w, fmt.Sprintf("%+v\n", x)); err != nil {
				break
			}
		}
	})
}

// signal that there are probably pod updates waiting to be processed
func (q *queuer) updatesAvailable() {
	q.deltaCond.Broadcast()
}

// delete a pod from the to-be-scheduled queue
func (q *queuer) dequeue(id string) {
	q.podQueue.Delete(id)
}

// re-add a pod to the to-be-scheduled queue, will not overwrite existing pod data (that
// may have already changed).
func (q *queuer) requeue(pod *Pod) {
	// use KeepExisting in case the pod has already been updated (can happen if binding fails
	// due to constraint voilations); we don't want to overwrite a newer entry with stale data.
	q.podQueue.Add(pod, queue.KeepExisting)
	q.unscheduledCond.Broadcast()
}

// same as requeue but calls podQueue.Offer instead of podQueue.Add
func (q *queuer) reoffer(pod *Pod) {
	// use KeepExisting in case the pod has already been updated (can happen if binding fails
	// due to constraint voilations); we don't want to overwrite a newer entry with stale data.
	if q.podQueue.Offer(pod, queue.KeepExisting) {
		q.unscheduledCond.Broadcast()
	}
}

// spawns a go-routine to watch for unscheduled pods and queue them up
// for scheduling. returns immediately.
func (q *queuer) Run() {
	go util.Forever(func() {
		log.Info("Watching for newly created pods")
		q.lock.Lock()
		defer q.lock.Unlock()

		for {
			// limit blocking here for short intervals so that scheduling
			// may proceed even if there have been no recent pod changes
			p := q.podUpdates.Await(enqueuePopTimeout)
			if p == nil {
				signalled := make(chan struct{})
				go func() {
					defer close(signalled)
					q.deltaCond.Wait()
				}()
				// we've yielded the lock
				select {
				case <-time.After(enqueueWaitTimeout):
					q.deltaCond.Broadcast() // abort Wait()
					<-signalled             // wait for lock re-acquisition
					log.V(4).Infoln("timed out waiting for a pod update")
				case <-signalled:
					// we've acquired the lock and there may be
					// changes for us to process now
				}
				continue
			}

			pod := p.(*Pod)
			if pod.Status.Host != "" {
				log.V(3).Infof("dequeuing pod for scheduling: %v", pod.Pod.Name)
				q.dequeue(pod.GetUID())
			} else {
				// use ReplaceExisting because we are always pushing the latest state
				now := time.Now()
				pod.deadline = &now
				if q.podQueue.Offer(pod, queue.ReplaceExisting) {
					q.unscheduledCond.Broadcast()
					log.V(3).Infof("queued pod for scheduling: %v", pod.Pod.Name)
				} else {
					log.Warningf("failed to queue pod for scheduling: %v", pod.Pod.Name)
				}
			}
		}
	}, 1*time.Second)
}

// implementation of scheduling plugin's NextPod func; see k8s plugin/pkg/scheduler
func (q *queuer) yield() *api.Pod {
	log.V(2).Info("attempting to yield a pod")
	q.lock.Lock()
	defer q.lock.Unlock()

	for {
		// limit blocking here to short intervals so that we don't block the
		// enqueuer Run() routine for very long
		kpod := q.podQueue.Await(yieldPopTimeout)
		if kpod == nil {
			signalled := make(chan struct{})
			go func() {
				defer close(signalled)
				q.unscheduledCond.Wait()
			}()

			// lock is yielded at this point and we're going to wait for either
			// a timeout, or a signal that there's data
			select {
			case <-time.After(yieldWaitTimeout):
				q.unscheduledCond.Broadcast() // abort Wait()
				<-signalled                   // wait for the go-routine, and the lock
				log.V(4).Infoln("timed out waiting for a pod to yield")
			case <-signalled:
				// we have acquired the lock, and there
				// may be a pod for us to pop now
			}
			continue
		}

		pod := kpod.(*Pod).Pod
		if podName, err := cache.MetaNamespaceKeyFunc(pod); err != nil {
			log.Warningf("yield unable to understand pod object %+v, will skip: %v", pod, err)
		} else if !q.podUpdates.Poll(podName, queue.POP_EVENT) {
			log.V(1).Infof("yield popped a transitioning pod, skipping: %+v", pod)
		} else if pod.Status.Host != "" {
			// should never happen if enqueuePods is filtering properly
			log.Warningf("yield popped an already-scheduled pod, skipping: %+v", pod)
		} else {
			return pod
		}
	}
}

type errorHandler struct {
	api     SchedulerInterface
	backoff *podBackoff
	qr      *queuer
}

// implementation of scheduling plugin's Error func; see plugin/pkg/scheduler
func (k *errorHandler) handleSchedulingError(pod *api.Pod, schedulingErr error) {

	if schedulingErr == noSuchPodErr {
		log.V(2).Infof("Not rescheduling non-existent pod %v", pod.Name)
		return
	}

	log.Infof("Error scheduling %v: %v; retrying", pod.Name, schedulingErr)
	defer util.HandleCrash()

	// default upstream scheduler passes pod.Name as binding.PodID
	ctx := api.WithNamespace(api.NewDefaultContext(), pod.Namespace)
	podKey, err := podtask.MakePodKey(ctx, pod.Name)
	if err != nil {
		log.Errorf("Failed to construct pod key, aborting scheduling for pod %v: %v", pod.Name, err)
		return
	}

	k.backoff.gc()
	k.api.RLocker().Lock()
	defer k.api.RLocker().Unlock()

	taskId, exists := k.api.taskForPod(podKey)
	if !exists {
		// if we don't have a mapping here any more then someone deleted the pod
		log.V(2).Infof("Could not resolve pod to task, aborting pod reschdule: %s", podKey)
		return
	}

	switch task, state := k.api.getTask(taskId); state {
	case podtask.StatePending:
		if task.Has(podtask.Launched) {
			log.V(2).Infof("Skipping re-scheduling for already-launched pod %v", podKey)
			return
		}
		breakoutEarly := queue.BreakChan(nil)
		if schedulingErr == noSuitableOffersErr {
			log.V(3).Infof("adding backoff breakout handler for pod %v", podKey)
			breakoutEarly = queue.BreakChan(k.api.offers().Listen(podKey, func(offer *mesos.Offer) bool {
				k.api.RLocker().Lock()
				defer k.api.RLocker().Unlock()
				switch task, state := k.api.getTask(taskId); state {
				case podtask.StatePending:
					return !task.Has(podtask.Launched) && task.AcceptOffer(offer)
				default:
					// no point in continuing to check for matching offers
					return true
				}
			}))
		}
		delay := k.backoff.getBackoff(podKey)
		log.V(3).Infof("requeuing pod %v with delay %v", podKey, delay)
		k.qr.requeue(&Pod{Pod: pod, delay: &delay, notify: breakoutEarly})
	default:
		log.V(2).Infof("Task is no longer pending, aborting reschedule for pod %v", podKey)
	}
}

type deleter struct {
	api SchedulerInterface
	qr  *queuer
}

// currently monitors for "pod deleted" events, upon which handle()
// is invoked.
func (k *deleter) Run(updates <-chan queue.Entry) {
	go util.Forever(func() {
		for {
			entry := <-updates
			pod := entry.Value().(*Pod)
			if entry.Is(queue.DELETE_EVENT) {
				if err := k.deleteOne(pod); err != nil {
					log.Error(err)
				}
			} else if !entry.Is(queue.POP_EVENT) {
				k.qr.updatesAvailable()
			}
		}
	}, 1*time.Second)
}

func (k *deleter) deleteOne(pod *Pod) error {
	ctx := api.WithNamespace(api.NewDefaultContext(), pod.Namespace)
	podKey, err := podtask.MakePodKey(ctx, pod.Name)
	if err != nil {
		return err
	}

	log.V(2).Infof("pod deleted: %v", podKey)

	// order is important here: we want to make sure we have the lock before
	// removing the pod from the scheduling queue. this makes the concurrent
	// execution of scheduler-error-handling and delete-handling easier to
	// reason about.
	k.api.Lock()
	defer k.api.Unlock()

	// prevent the scheduler from attempting to pop this; it's also possible that
	// it's concurrently being scheduled (somewhere between pod scheduling and
	// binding) - if so, then we'll end up removing it from taskRegistry which
	// will abort Bind()ing
	k.qr.dequeue(pod.GetUID())

	taskId, exists := k.api.taskForPod(podKey)
	if !exists {
		log.V(2).Infof("Could not resolve pod '%s' to task id", podKey)
		return noSuchPodErr
	}

	// determine if the task has already been launched to mesos, if not then
	// cleanup is easier (unregister) since there's no state to sync
	switch task, state := k.api.getTask(taskId); state {
	case podtask.StatePending:
		if !task.Has(podtask.Launched) {
			// we've been invoked in between Schedule() and Bind()
			if task.HasAcceptedOffer() {
				task.Offer.Release()
				task.ClearTaskInfo()
			}
			k.api.unregisterPodTask(task)
			return nil
		}
		fallthrough
	case podtask.StateRunning:
		// signal to watchers that the related pod is going down
		task.Set(podtask.Deleted)
		return k.api.killTask(taskId)
	default:
		log.Infof("cannot kill pod '%s': task not found %v", podKey, taskId)
		return noSuchTaskErr
	}
}

// Create creates a scheduler plugin and all supporting background functions.
func (k *KubernetesScheduler) NewPluginConfig(startLatch <-chan struct{}) *PluginConfig {

	// Watch and queue pods that need scheduling.
	updates := make(chan queue.Entry, defaultUpdatesBacklog)
	podUpdates := &podStoreAdapter{queue.NewHistorical(updates)}
	reflector := cache.NewReflector(createAllPodsLW(k.client), &api.Pod{}, podUpdates)

	// lock that guards critial sections that involve transferring pods from
	// the store (cache) to the scheduling queue; its purpose is to maintain
	// an ordering (vs interleaving) of operations that's easier to reason about.
	kapi := &k8smScheduler{k}
	q := newQueuer(podUpdates)
	podDeleter := &deleter{
		api: kapi,
		qr:  q,
	}
	eh := &errorHandler{
		api: kapi,
		backoff: &podBackoff{
			perPodBackoff:   map[string]*backoffEntry{},
			clock:           realClock{},
			defaultDuration: 1 * time.Second,
			maxDuration:     60 * time.Second,
		},
		qr: q,
	}
	go func() {
		select {
		case <-startLatch:
			reflector.Run()
			podDeleter.Run(updates)
			q.Run()
		}
	}()
	q.installDebugHandlers()
	podtask.InstallDebugHandlers(k.RLocker(), k.taskRegistry)
	return &PluginConfig{
		Config: &plugin.Config{
			MinionLister: nil,
			Algorithm: &kubeScheduler{
				api:        kapi,
				podUpdates: podUpdates,
			},
			Binder: &binder{
				api:    kapi,
				client: k.client,
			},
			NextPod: q.yield,
			Error:   eh.handleSchedulingError,
		},
		api:     kapi,
		client:  k.client,
		qr:      q,
		deleter: podDeleter,
	}
}

type PluginConfig struct {
	*plugin.Config
	api     SchedulerInterface
	client  *client.Client
	qr      *queuer
	deleter *deleter
}

func NewPlugin(c *PluginConfig) PluginInterface {
	return &schedulingPlugin{
		Scheduler: plugin.New(c.Config),
		api:       c.api,
		client:    c.client,
		qr:        c.qr,
		deleter:   c.deleter,
	}
}

type schedulingPlugin struct {
	*plugin.Scheduler
	api     SchedulerInterface
	client  *client.Client
	qr      *queuer
	deleter *deleter
}

// this pod may be out of sync with respect to the API server registry:
//      this pod   |  apiserver registry
//    -------------|----------------------
//      host=.*    |  404           ; pod was deleted
//      host=.*    |  5xx           ; failed to sync, try again later?
//      host=""    |  host=""       ; perhaps no updates to process?
//      host=""    |  host="..."    ; pod has been scheduled and assigned, is there a task assigned? (check TaskIdKey in binding?)
//      host="..." |  host=""       ; pod is no longer scheduled, does it need to be re-queued?
//      host="..." |  host="..."    ; perhaps no updates to process?
//
// TODO(jdef) this needs an integration test
func (s *schedulingPlugin) reconcilePod(oldPod api.Pod) {
	log.V(1).Infof("reconcile pod %v", oldPod.Name)
	ctx := api.WithNamespace(api.NewDefaultContext(), oldPod.Namespace)
	pod, err := s.client.Pods(api.NamespaceValue(ctx)).Get(oldPod.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			// attempt to delete
			if err = s.deleter.deleteOne(&Pod{Pod: &oldPod}); err != nil && err != noSuchPodErr && err != noSuchTaskErr {
				log.Errorf("failed to delete pod: %v: %v", oldPod.Name, err)
			}
		} else {
			//TODO(jdef) other errors should probably trigger a retry (w/ backoff).
			//For now, drop the pod on the floor
			log.Warning("aborting reconciliation for pod %v: %v", oldPod.Name, err)
		}
		return
	}
	if oldPod.Status.Host != pod.Status.Host {
		if pod.Status.Host == "" {
			// pod is unscheduled.
			// it's possible that we dropped the pod in the scheduler error handler
			// because of task misalignment with the pod (task.Has(podtask.Launched) == true)

			podKey, err := podtask.MakePodKey(ctx, pod.Name)
			if err != nil {
				log.Error(err)
				return
			}

			s.api.Lock()
			defer s.api.Unlock()

			if _, exists := s.api.taskForPod(podKey); exists {
				//TODO(jdef) reconcile the task
				log.Errorf("task already registered for pod %v", pod.Name)
				return
			}

			now := time.Now()
			log.V(3).Infof("reoffering pod %v", podKey)
			s.qr.reoffer(&Pod{
				Pod:      pod,
				deadline: &now,
			})
		} else {
			// pod is scheduled.
			// not sure how this happened behind our backs. attempt to reconstruct
			// at least a partial podtask.T record.
			//TODO(jdef) reconcile the task
			log.Errorf("pod already scheduled: %v", pod.Name)
		}
	} else {
		//TODO(jdef) for now, ignore the fact that the rest of the spec may be different
		//and assume that our knowledge of the pod aligns with that of the apiserver
		log.Error("pod reconciliation does not support updates; not yet implemented")
	}
}

type listWatch struct {
	client        *client.Client
	fieldSelector labels.Selector
	resource      string
}

func (lw *listWatch) List() (runtime.Object, error) {
	return lw.client.
		Get().
		Resource(lw.resource).
		SelectorParam("fields", lw.fieldSelector).
		Do().
		Get()
}

func (lw *listWatch) Watch(resourceVersion string) (watch.Interface, error) {
	return lw.client.
		Get().
		Prefix("watch").
		Resource(lw.resource).
		SelectorParam("fields", lw.fieldSelector).
		Param("resourceVersion", resourceVersion).
		Watch()
}

// createAllPodsLW returns a listWatch that finds all pods
func createAllPodsLW(cl *client.Client) *listWatch {
	return &listWatch{
		client:        cl,
		fieldSelector: labels.Everything(),
		resource:      "pods",
	}
}

// Consumes *api.Pod, produces *Pod; the k8s reflector wants to push *api.Pod
// objects at us, but we want to store more flexible (Pod) type defined in
// this package. The adapter implementation facilitates this. It's a little
// hackish since the object type going in is different than the object type
// coming out -- you've been warned.
type podStoreAdapter struct {
	queue.FIFO
}

func (psa *podStoreAdapter) Add(obj interface{}) error {
	pod := obj.(*api.Pod)
	return psa.FIFO.Add(&Pod{Pod: pod})
}

func (psa *podStoreAdapter) Update(obj interface{}) error {
	pod := obj.(*api.Pod)
	return psa.FIFO.Update(&Pod{Pod: pod})
}

func (psa *podStoreAdapter) Delete(obj interface{}) error {
	pod := obj.(*api.Pod)
	return psa.FIFO.Delete(&Pod{Pod: pod})
}

func (psa *podStoreAdapter) Get(obj interface{}) (interface{}, bool, error) {
	pod := obj.(*api.Pod)
	return psa.FIFO.Get(&Pod{Pod: pod})
}

// Replace will delete the contents of the store, using instead the
// given map. This store implementation does NOT take ownership of the map.
func (psa *podStoreAdapter) Replace(objs []interface{}) error {
	newobjs := make([]interface{}, len(objs))
	for i, v := range objs {
		pod := v.(*api.Pod)
		newobjs[i] = &Pod{Pod: pod}
	}
	return psa.FIFO.Replace(newobjs)
}
