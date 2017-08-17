package ingress

import (
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"

	api "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/client-go/tools/record"

	listenersP "github.com/coreos/alb-ingress-controller/pkg/alb/listeners"
	"github.com/coreos/alb-ingress-controller/pkg/alb/loadbalancer"
	"github.com/coreos/alb-ingress-controller/pkg/alb/targetgroups"
	"github.com/coreos/alb-ingress-controller/pkg/annotations"
	albelbv2 "github.com/coreos/alb-ingress-controller/pkg/aws/elbv2"
	"github.com/coreos/alb-ingress-controller/pkg/util/log"
	util "github.com/coreos/alb-ingress-controller/pkg/util/types"
)

var logger *log.Logger

func init() {
	logger = log.New("ingress")
}

// ALBIngress contains all information above the cluster, ingress resource, and AWS resources
// needed to assemble an ALB, TargetGroup, Listener and Rules.
type ALBIngress struct {
	Id           *string
	namespace    *string
	ingressName  *string
	clusterName  *string
	recorder     record.EventRecorder
	ingress      *extensions.Ingress
	lock         *sync.Mutex
	annotations  *annotations.Annotations
	LoadBalancer *loadbalancer.LoadBalancer
	valid        bool
	logger       *log.Logger
}

type NewALBIngressOptions struct {
	Namespace   string
	Name        string
	ClusterName string
	Recorder    record.EventRecorder
}

// ID returns an ingress id based off of a namespace and name
func ID(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

// NewALBIngress returns a minimal ALBIngress instance with a generated name that allows for lookup
// when new ALBIngress objects are created to determine if an instance of that ALBIngress already
// exists.
func NewALBIngress(o *NewALBIngressOptions) *ALBIngress {
	ingressID := ID(o.Namespace, o.Name)
	return &ALBIngress{
		Id:          aws.String(ingressID),
		namespace:   aws.String(o.Namespace),
		clusterName: aws.String(o.ClusterName),
		ingressName: aws.String(o.Name),
		lock:        new(sync.Mutex),
		logger:      log.New(ingressID),
		recorder:    o.Recorder,
	}
}

type NewALBIngressFromIngressOptions struct {
	Ingress            *extensions.Ingress
	ExistingIngress    *ALBIngress
	ClusterName        string
	GetServiceNodePort func(string, int32) (*int64, error)
	GetNodes           func() util.AWSStringSlice
	Recorder           record.EventRecorder
}

// NewALBIngressFromIngress builds ALBIngress's based off of an Ingress object
// https://godoc.org/k8s.io/kubernetes/pkg/apis/extensions#Ingress. Creates a new ingress object,
// and looks up to see if a previous ingress object with the same id is known to the ALBController.
// If there is an issue and the ingress is invalid, nil is returned.
func NewALBIngressFromIngress(o *NewALBIngressFromIngressOptions) *ALBIngress {
	var err error

	// Create newIngress ALBIngress object holding the resource details and some cluster information.
	newIngress := NewALBIngress(&NewALBIngressOptions{
		Namespace:   o.Ingress.GetNamespace(),
		Name:        o.Ingress.Name,
		ClusterName: o.ClusterName,
		Recorder:    o.Recorder,
	})

	if o.ExistingIngress != nil {
		// Acquire a lock to prevent race condition if existing ingress's state is currently being synced
		// with Amazon..
		newIngress = o.ExistingIngress
		newIngress.lock.Lock()
		defer newIngress.lock.Unlock()
		// Ensure all desired state is removed from the copied ingress. The desired state of each
		// component will be generated later in this function.
		newIngress.StripDesiredState()
	}
	newIngress.ingress = o.Ingress

	// Load up the ingress with our current annotations.
	newIngress.annotations, err = annotations.ParseAnnotations(o.Ingress.Annotations)
	if err != nil {
		msg := fmt.Sprintf("Error parsing annotations: %s", err.Error())
		newIngress.Eventf(api.EventTypeWarning, "ERROR", msg)
		newIngress.logger.Errorf(msg)
		return newIngress
	}

	// If annotation set is nil, its because it was cached as an invalid set before. Stop processing
	// and return nil.
	if newIngress.annotations == nil {
		msg := fmt.Sprintf("Skipping processing due to a history of bad annotations")
		newIngress.Eventf(api.EventTypeWarning, "ERROR", msg)
		newIngress.logger.Debugf(msg)
		return newIngress
	}

	// Assemble the load balancer
	newLoadBalancer := loadbalancer.NewLoadBalancer(o.ClusterName, o.Ingress.GetNamespace(), o.Ingress.Name, newIngress.logger, newIngress.annotations, newIngress.Tags())
	if newIngress.LoadBalancer != nil {
		// we had an existing LoadBalancer in ingress, so just copy the desired state over
		newIngress.LoadBalancer.DesiredLoadBalancer = newLoadBalancer.DesiredLoadBalancer
		newIngress.LoadBalancer.DesiredTags = newLoadBalancer.DesiredTags
	} else {
		// no existing LoadBalancer, so use the one we just created
		newIngress.LoadBalancer = newLoadBalancer
	}
	lb := newIngress.LoadBalancer

	// Assemble the target groups
	lb.TargetGroups, err = targetgroups.NewTargetGroupsFromIngress(&targetgroups.NewTargetGroupsFromIngressOptions{
		Ingress:              o.Ingress,
		LoadBalancerID:       newIngress.LoadBalancer.ID,
		ExistingTargetGroups: newIngress.LoadBalancer.TargetGroups,
		Annotations:          newIngress.annotations,
		ClusterName:          &o.ClusterName,
		Namespace:            o.Ingress.GetNamespace(),
		Tags:                 newIngress.Tags(),
		Logger:               newIngress.logger,
		GetServiceNodePort:   o.GetServiceNodePort,
		GetNodes:             o.GetNodes,
	})
	if err != nil {
		msg := fmt.Sprintf("Error instantiating target groups: %s", err.Error())
		newIngress.Eventf(api.EventTypeWarning, "ERROR", msg)
		newIngress.logger.Errorf(msg)
		return newIngress
	}

	// Assemble the listeners
	lb.Listeners, err = listenersP.NewListenersFromIngress(&listenersP.NewListenersFromIngressOptions{
		Ingress:     o.Ingress,
		Listeners:   newIngress.LoadBalancer.Listeners,
		Annotations: newIngress.annotations,
		Logger:      newIngress.logger,
	})
	if err != nil {
		msg := fmt.Sprintf("Error instantiating listeners: %s", err.Error())
		newIngress.Eventf(api.EventTypeWarning, "ERROR", msg)
		newIngress.logger.Errorf(msg)
		return newIngress
	}

	newIngress.valid = true
	return newIngress
}

type NewALBIngressFromAWSLoadBalancerOptions struct {
	LoadBalancer *elbv2.LoadBalancer
	ClusterName  string
	Recorder     record.EventRecorder
}

// NewALBIngressFromAWSLoadBalancer builds ALBIngress's based off of an elbv2.LoadBalancer
func NewALBIngressFromAWSLoadBalancer(o *NewALBIngressFromAWSLoadBalancerOptions) (*ALBIngress, error) {
	logger.Debugf("Fetching Tags for %s", *o.LoadBalancer.LoadBalancerArn)
	tags, err := albelbv2.ELBV2svc.DescribeTagsForArn(o.LoadBalancer.LoadBalancerArn)
	if err != nil {
		return nil, fmt.Errorf("Unable to get tags for %s: %s", *o.LoadBalancer.LoadBalancerName, err.Error())
	}

	ingressName, ok := tags.Get("IngressName")
	if !ok {
		return nil, fmt.Errorf("The LoadBalancer %s does not have an IngressName tag, can't import", *o.LoadBalancer.LoadBalancerName)
	}

	namespace, ok := tags.Get("Namespace")
	if !ok {
		return nil, fmt.Errorf("The LoadBalancer %s does not have an Namespace tag, can't import", *o.LoadBalancer.LoadBalancerName)
	}

	// Assemble ingress
	ingress := NewALBIngress(&NewALBIngressOptions{
		Namespace:   namespace,
		Name:        ingressName,
		ClusterName: o.ClusterName,
		Recorder:    o.Recorder,
	})

	// Assemble load balancer
	ingress.LoadBalancer, err = loadbalancer.NewLoadBalancerFromAWSLoadBalancer(o.LoadBalancer, tags, o.ClusterName, ingress.logger)
	if err != nil {
		return nil, err
	}

	// Assemble target groups
	targetGroups, err := albelbv2.ELBV2svc.DescribeTargetGroupsForLoadBalancer(o.LoadBalancer.LoadBalancerArn)
	if err != nil {
		return nil, err
	}

	ingress.LoadBalancer.TargetGroups, err = targetgroups.NewTargetGroupsFromAWSTargetGroups(targetGroups, o.ClusterName, ingress.LoadBalancer.ID, ingress.logger)
	if err != nil {
		return nil, err
	}

	// Assemble listeners
	listeners, err := albelbv2.ELBV2svc.DescribeListenersForLoadBalancer(o.LoadBalancer.LoadBalancerArn)
	if err != nil {
		return nil, err
	}

	ingress.LoadBalancer.Listeners, err = listenersP.NewListenersFromAWSListeners(listeners, ingress.logger)
	if err != nil {
		return nil, err
	}

	// Assembly complete
	logger.Infof("Ingress rebuilt from existing ALB in AWS")
	ingress.valid = true
	return ingress, nil
}

// Eventf writes an event to the ALBIngress's Kubernetes ingress resource
func (a *ALBIngress) Eventf(eventtype, reason, messageFmt string, args ...interface{}) {
	if a.ingress == nil || a.recorder == nil {
		return
	}
	a.recorder.Eventf(a.ingress, eventtype, reason, messageFmt, args...)
}

// Hostname returns the AWS hostnames for the load balancer
func (a *ALBIngress) Hostnames() ([]api.LoadBalancerIngress, error) {
	var hostnames []api.LoadBalancerIngress

	if a.LoadBalancer == nil {
		return hostnames, nil
	}

	if a.LoadBalancer.CurrentLoadBalancer != nil && a.LoadBalancer.CurrentLoadBalancer.DNSName != nil {
		hostnames = append(hostnames, api.LoadBalancerIngress{Hostname: *a.LoadBalancer.CurrentLoadBalancer.DNSName})
	}

	if len(hostnames) == 0 {
		a.logger.Errorf("No ALB hostnames for ingress")
		return nil, fmt.Errorf("No ALB hostnames for ingress")
	}

	return hostnames, nil
}

// Reconcile begins the state sync for all AWS resource satisfying this ALBIngress instance.
func (a *ALBIngress) Reconcile(rOpts *ReconcileOptions) {
	a.lock.Lock()
	defer a.lock.Unlock()
	// If the ingress resource is invalid, don't attempt reconcile
	if !a.valid {
		return
	}

	errors := a.LoadBalancer.Reconcile(loadbalancer.NewReconcileOptions().SetEventf(rOpts.Eventf).SetLoadBalancer(a.LoadBalancer))
	if len(errors) > 0 {
		a.logger.Errorf("Failed to reconcile state on this ingress")
		for _, err := range errors {
			a.logger.Errorf(" - %s", err.Error())
		}
	}
}

// Name returns the name of the ingress
func (a *ALBIngress) Name() string {
	return fmt.Sprintf("%s-%s", *a.namespace, *a.ingressName)
}

// StripDesiredState strips all desired objects from an ALBIngress
func (a *ALBIngress) StripDesiredState() {
	if a.LoadBalancer != nil {
		a.LoadBalancer.StripDesiredState()
	}
}

// Tags returns an elbv2.Tag slice of standard tags for the ingress AWS resources
func (a *ALBIngress) Tags() []*elbv2.Tag {
	tags := a.annotations.Tags

	tags = append(tags, &elbv2.Tag{
		Key:   aws.String("Namespace"),
		Value: a.namespace,
	})

	tags = append(tags, &elbv2.Tag{
		Key:   aws.String("IngressName"),
		Value: a.ingressName,
	})

	return tags
}

type ReconcileOptions struct {
	Eventf func(string, string, string, ...interface{})
}

func NewReconcileOptions() *ReconcileOptions {
	return &ReconcileOptions{}
}

func (r *ReconcileOptions) SetEventf(f func(string, string, string, ...interface{})) *ReconcileOptions {
	r.Eventf = f
	return r
}
