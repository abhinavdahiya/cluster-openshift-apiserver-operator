package configobserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/imdario/mergo"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/rand"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/client-go/util/workqueue"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/common"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const operatorStatusTypeConfigObservationFailing = "ConfigObservationFailing"
const configObserverWorkKey = "key"

// Listers is an interface which will be passed to the config observer funcs.  It is expected to be hard-cast to the "correct" type
type Listers interface {
	PreRunHasSynced() []cache.InformerSynced
}

// ObserveConfigFunc observes configuration and returns the observedConfig. This function should not return an
// observedConfig that would cause the service being managed by the operator to crash. For example, if a required
// configuration key cannot be observed, consider reusing the configuration key's previous value. Errors that occur
// while attempting to generate the observedConfig should be returned in the errs slice.
type ObserveConfigFunc func(listers Listers, recorder events.Recorder, existingConfig map[string]interface{}) (observedConfig map[string]interface{}, errs []error)

type ConfigObserver struct {
	operatorConfigClient v1helpers.OperatorClient
	eventRecorder        events.Recorder

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue workqueue.RateLimitingInterface

	rateLimiter flowcontrol.RateLimiter
	// observers are called in an undefined order and their results are merged to
	// determine the observed configuration.
	observers []ObserveConfigFunc

	// listers are used by config observers to retrieve necessary resources
	listers Listers
}

func NewConfigObserver(
	operatorClient v1helpers.OperatorClient,
	eventRecorder events.Recorder,
	listers Listers,
	observers ...ObserveConfigFunc,
) *ConfigObserver {
	return &ConfigObserver{
		operatorConfigClient: operatorClient,
		eventRecorder:        eventRecorder,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ConfigObserver"),

		rateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.05 /*3 per minute*/, 4),
		observers:   observers,
		listers:     listers,
	}
}

// sync reacts to a change in prereqs by finding information that is required to match another value in the cluster. This
// must be information that is logically "owned" by another component.
func (c ConfigObserver) sync() error {
	originalSpec, _, resourceVersion, err := c.operatorConfigClient.GetOperatorState()
	if err != nil {
		return err
	}
	spec := originalSpec.DeepCopy()

	// don't worry about errors.  If we can't decode, we'll simply stomp over the field.
	existingConfig := map[string]interface{}{}
	if err := json.NewDecoder(bytes.NewBuffer(spec.ObservedConfig.Raw)).Decode(&existingConfig); err != nil {
		glog.V(4).Infof("decode of existing config failed with error: %v", err)
	}

	var errs []error
	var observedConfigs []map[string]interface{}
	for _, i := range rand.Perm(len(c.observers)) {
		var currErrs []error
		observedConfig, currErrs := c.observers[i](c.listers, c.eventRecorder, existingConfig)
		observedConfigs = append(observedConfigs, observedConfig)
		errs = append(errs, currErrs...)
	}

	mergedObservedConfig := map[string]interface{}{}
	for _, observedConfig := range observedConfigs {
		if err := mergo.Merge(&mergedObservedConfig, observedConfig); err != nil {
			glog.Warningf("merging observed config failed: %v", err)
		}
	}

	if !equality.Semantic.DeepEqual(existingConfig, mergedObservedConfig) {
		glog.Infof("writing updated observedConfig: %v", diff.ObjectDiff(existingConfig, mergedObservedConfig))
		spec.ObservedConfig = runtime.RawExtension{Object: &unstructured.Unstructured{Object: mergedObservedConfig}}
		_, resourceVersion, err = c.operatorConfigClient.UpdateOperatorSpec(resourceVersion, spec)
		if err != nil {
			errs = append(errs, fmt.Errorf("error writing updated observed config: %v", err))
			c.eventRecorder.Warningf("ObservedConfigWriteError", "Failed to write observed config: %v", err)
		} else {
			c.eventRecorder.Eventf("ObservedConfigChanged", "Writing updated observed config")
		}
	}
	err = common.NewMultiLineAggregate(errs)

	// update failing condition
	cond := operatorv1.OperatorCondition{
		Type:   operatorStatusTypeConfigObservationFailing,
		Status: operatorv1.ConditionFalse,
	}
	if err != nil {
		cond.Status = operatorv1.ConditionTrue
		cond.Reason = "Error"
		cond.Message = err.Error()
	}
	if _, updateError := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(cond)); updateError != nil {
		return updateError
	}

	// explicitly ignore errs, we are requeued by input changes anyway
	return nil
}

func (c *ConfigObserver) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	glog.Infof("Starting ConfigObserver")
	defer glog.Infof("Shutting down ConfigObserver")

	if !cache.WaitForCacheSync(stopCh, c.listers.PreRunHasSynced()...) {
		utilruntime.HandleError(fmt.Errorf("caches did not sync"))
		return
	}

	// doesn't matter what workers say, only start one.
	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *ConfigObserver) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *ConfigObserver) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	// before we call sync, we want to wait for token.  We do this to avoid hot looping.
	c.rateLimiter.Accept()

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// eventHandler queues the operator to check spec and status
func (c *ConfigObserver) EventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(configObserverWorkKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(configObserverWorkKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(configObserverWorkKey) },
	}
}
