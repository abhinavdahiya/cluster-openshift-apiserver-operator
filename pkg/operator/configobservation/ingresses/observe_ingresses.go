package ingresses

import (
	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/openshift/cluster-openshift-apiserver-operator/pkg/operator/configobservation"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"
)

func ObserveIngressDomain(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
	listers := genericListers.(configobservation.Listers)
	var errs []error
	prevObservedConfig := map[string]interface{}{}

	routingConfigSubdomainPath := []string{"routingConfig", "subdomain"}
	currentRoutingDomain, _, err := unstructured.NestedString(existingConfig, routingConfigSubdomainPath...)
	if err != nil {
		return prevObservedConfig, append(errs, err)
	}
	if len(currentRoutingDomain) > 0 {
		err := unstructured.SetNestedField(prevObservedConfig, currentRoutingDomain, routingConfigSubdomainPath...)
		if err != nil {
			return prevObservedConfig, append(errs, err)
		}
	}

	if !listers.IngressConfigSynced() {
		glog.Warning("ingresses.config.openshift.io not synced")
		return prevObservedConfig, errs
	}

	observedConfig := map[string]interface{}{}
	configIngress, err := listers.IngressConfigLister.Get("cluster")
	if errors.IsNotFound(err) {
		glog.Warningf("ingress.config.openshift.io/default: not found")
		return observedConfig, errs
	}
	if err != nil {
		return prevObservedConfig, append(errs, err)
	}

	routingDomain := configIngress.Spec.Domain
	if len(routingDomain) > 0 {
		err = unstructured.SetNestedField(observedConfig, routingDomain, routingConfigSubdomainPath...)
		if err != nil {
			return prevObservedConfig, append(errs, err)
		}
	}

	return observedConfig, errs
}
