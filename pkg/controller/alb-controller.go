package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	awselbv2 "github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/spf13/pflag"

	api "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/client-go/tools/record"
	"k8s.io/ingress/core/pkg/ingress"
	"k8s.io/ingress/core/pkg/ingress/annotations/class"
	"k8s.io/ingress/core/pkg/ingress/controller"
	"k8s.io/ingress/core/pkg/ingress/defaults"

	"github.com/coreos/alb-ingress-controller/pkg/aws/acm"
	"github.com/coreos/alb-ingress-controller/pkg/aws/ec2"
	"github.com/coreos/alb-ingress-controller/pkg/aws/elbv2"
	"github.com/coreos/alb-ingress-controller/pkg/aws/iam"
	"github.com/coreos/alb-ingress-controller/pkg/aws/session"
	"github.com/coreos/alb-ingress-controller/pkg/config"
	albingress "github.com/coreos/alb-ingress-controller/pkg/ingress"
	albprom "github.com/coreos/alb-ingress-controller/pkg/prometheus"
	"github.com/coreos/alb-ingress-controller/pkg/util/log"
	util "github.com/coreos/alb-ingress-controller/pkg/util/types"
)

// ALBController is our main controller
type ALBController struct {
	storeLister  ingress.StoreLister
	recorder     record.EventRecorder
	ALBIngresses albingress.ALBIngressesT
	clusterName  string
	IngressClass string
}

var logger *log.Logger

func init() {
	logger = log.New("controller")
}

// NewALBController returns an ALBController
func NewALBController(awsconfig *aws.Config, conf *config.Config) *ALBController {
	ac := new(ALBController)
	sess := session.NewSession(awsconfig, conf.AWSDebug)
	elbv2.NewELBV2(sess)
	ec2.NewEC2(sess)
	acm.NewACM(sess)
	iam.NewIAM(sess)

	return ingress.Controller(ac).(*ALBController)
}

func (ac *ALBController) Configure(ic *controller.GenericController) {
	ac.IngressClass = ic.IngressClass()
	if ac.IngressClass != "" {
		logger.Infof("Ingress class set to %s", ac.IngressClass)
	}

	if len(ac.clusterName) > 11 {
		logger.Exitf("Cluster name must be 11 characters or less")
	}

	if ac.clusterName == "" {
		logger.Exitf("A cluster name must be defined")
	}

	if strings.Contains(ac.clusterName, "-") {
		logger.Exitf("Cluster name cannot contain '-'")
	}

	ac.recorder = ic.GetRecoder()
}

// OnUpdate is a callback invoked from the sync queue when ingress resources, or resources ingress
// resources touch, change. On each new event a new list of ALBIngresses are created and evaluated
// against the existing ALBIngress list known to the ALBController. Eventually the state of this
// list is synced resulting in new ingresses causing resource creation, modified ingresses having
// resources modified (when appropriate) and ingresses missing from the new list deleted from AWS.
func (ac *ALBController) OnUpdate(_ ingress.Configuration) error {
	albprom.OnUpdateCount.Add(float64(1))

	logger.Debugf("OnUpdate event seen by ALB ingress controller.")

	// Create new ALBIngress list for this invocation.
	var ALBIngresses albingress.ALBIngressesT
	// Find every ingress currently in Kubernetes.
	for _, ingress := range ac.storeLister.Ingress.List() {
		ingResource := ingress.(*extensions.Ingress)
		// Ensure the ingress resource found contains an appropriate ingress class.
		if !class.IsValid(ingResource, ac.IngressClass, ac.DefaultIngressClass()) {
			continue
		}
		// Produce a new ALBIngress instance for every ingress found. If ALBIngress returns nil, there
		// was an issue with the ingress (e.g. bad annotations) and should not be added to the list.
		ALBIngress, err := albingress.NewALBIngressFromIngress(&albingress.NewALBIngressFromIngressOptions{
			Ingress:            ingResource,
			ExistingIngresses:  ac.ALBIngresses,
			ClusterName:        ac.clusterName,
			GetServiceNodePort: ac.GetServiceNodePort,
			GetNodes:           ac.GetNodes,
			Recorder:           ac.recorder,
		})
		if ALBIngress == nil {
			continue
		}
		if err != nil {
			ALBIngress.Tainted = true
		}
		// Add the new ALBIngress instance to the new ALBIngress list.
		ALBIngresses = append(ALBIngresses, ALBIngress)
	}

	// Capture any ingresses missing from the new list that qualify for deletion.
	deletable := ac.ingressToDelete(ALBIngresses)
	// If deletable ingresses were found, add them to the list so they'll be deleted when Reconcile()
	// is called.
	if len(deletable) > 0 {
		ALBIngresses = append(ALBIngresses, deletable...)
	}

	albprom.ManagedIngresses.Set(float64(len(ALBIngresses)))
	// Update the list of ALBIngresses known to the ALBIngress controller to the newly generated list.
	ac.ALBIngresses = ALBIngresses

	// Sync the state, resulting in creation, modify, delete, or no action, for every ALBIngress
	// instance known to the ALBIngress controller.
	var wg sync.WaitGroup
	wg.Add(len(ac.ALBIngresses))
	for _, ingress := range ac.ALBIngresses {
		go func(wg *sync.WaitGroup, ingress *albingress.ALBIngress) {
			defer wg.Done()
			ingress.Reconcile(albingress.NewReconcileOptions().SetEventf(ingress.Eventf))
		}(&wg, ingress)
	}
	wg.Wait()

	return nil
}

// OverrideFlags configures optional override flags for the ingress controller
func (ac *ALBController) OverrideFlags(flags *pflag.FlagSet) {
	flags.Set("update-status-on-shutdown", "false")
}

// SetConfig configures a configmap for the ingress controller
func (ac *ALBController) SetConfig(cfgMap *api.ConfigMap) {
}

// SetListers sets the configured store listers in the generic ingress controller
func (ac *ALBController) SetListers(lister ingress.StoreLister) {
	ac.storeLister = lister
}

// BackendDefaults returns default configurations for the backend
func (ac *ALBController) BackendDefaults() defaults.Backend {
	var backendDefaults defaults.Backend
	return backendDefaults
}

// Name returns the ingress controller name
func (ac *ALBController) Name() string {
	return "AWS Application Load Balancer Controller"
}

// Check tests the ingress controller configuration
func (ac *ALBController) Check(_ *http.Request) error {
	return nil
}

// DefaultIngressClass returns thed default ingress class
func (ac *ALBController) DefaultIngressClass() string {
	return "alb"
}

// Info returns information on the ingress contoller
func (ac *ALBController) Info() *ingress.BackendInfo {
	return &ingress.BackendInfo{
		Name:       "ALB Ingress Controller",
		Release:    "1.0.0",
		Build:      "git-00000000",
		Repository: "git://github.com/coreos/alb-ingress-controller",
	}
}

// ConfigureFlags
func (ac *ALBController) ConfigureFlags(pf *pflag.FlagSet) {
	pf.StringVar(&ac.clusterName, "clusterName", os.Getenv("CLUSTER_NAME"), "Cluster Name (required)")
}

func (ac *ALBController) UpdateIngressStatus(ing *extensions.Ingress) []api.LoadBalancerIngress {
	ingress := albingress.NewALBIngress(&albingress.NewALBIngressOptions{
		Namespace:   ing.ObjectMeta.Namespace,
		Name:        ing.ObjectMeta.Name,
		ClusterName: ac.clusterName,
		Recorder:    ac.recorder,
	})

	i := ac.ALBIngresses.Find(ingress)
	if i < 0 {
		logger.Errorf("Unable to find ingress %s", ingress.Name())
		return nil
	}

	hostnames, err := ac.ALBIngresses[i].Hostnames()
	if err != nil {
		return nil
	}

	return hostnames
}

// GetServiceNodePort returns the nodeport for a given Kubernetes service
func (ac *ALBController) GetServiceNodePort(serviceKey string, backendPort int32) (*int64, error) {
	// Verify the service (namespace/service-name) exists in Kubernetes.
	item, exists, _ := ac.storeLister.Service.GetByKey(serviceKey)
	if !exists {
		return nil, fmt.Errorf("Unable to find the %v service", serviceKey)
	}

	// Verify the service type is Node port.
	if item.(*api.Service).Spec.Type != api.ServiceTypeNodePort {
		return nil, fmt.Errorf("%v service is not of type NodePort", serviceKey)

	}

	// Find associated target port to ensure correct NodePort is assigned.
	for _, p := range item.(*api.Service).Spec.Ports {
		if p.Port == backendPort {
			return aws.Int64(int64(p.NodePort)), nil
		}
	}

	return nil, fmt.Errorf("Unable to find a port defined in the %v service", serviceKey)
}

// Returns a list of ingress objects that are no longer known to kubernetes and should
// be deleted.
// TODO: Move to ingress
func (ac *ALBController) ingressToDelete(newList albingress.ALBIngressesT) albingress.ALBIngressesT {
	var deleteableIngress albingress.ALBIngressesT

	// Loop through every ingress in current (old) ingress list known to ALBController
	for _, ingress := range ac.ALBIngresses {
		// If assembling the ingress resource failed, don't attempt deletion
		if ingress.Tainted {
			continue
		}
		// Ingress objects not found in newList might qualify for deletion.
		if i := newList.Find(ingress); i < 0 {
			// If the ALBIngress still contains a LoadBalancer, it still needs to be deleted.
			// In this case, strip all desired state and add it to the deleteableIngress list.
			// If the ALBIngress contains no LoadBalancer, it was previously deleted and is
			// no longer relevant to the ALBController.
			if ingress.LoadBalancer != nil {
				ingress.StripDesiredState()
				deleteableIngress = append(deleteableIngress, ingress)
			}
		}
	}
	return deleteableIngress
}

func (ac *ALBController) StateHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ac.ALBIngresses)
}

// AssembleIngresses builds a list of existing ingresses from resources in AWS
func (ac *ALBController) AssembleIngresses() {
	logger.Infof("Build up list of existing ingresses")
	var ingresses albingress.ALBIngressesT

	loadBalancers, err := elbv2.ELBV2svc.GetClusterLoadBalancers(&ac.clusterName)
	if err != nil {
		logger.Fatalf(err.Error())
	}

	var wg sync.WaitGroup
	wg.Add(len(loadBalancers))

	for _, loadBalancer := range loadBalancers {
		go func(wg *sync.WaitGroup, loadBalancer *awselbv2.LoadBalancer) {
			defer wg.Done()

			albIngress, err := albingress.NewALBIngressFromAWSLoadBalancer(&albingress.NewALBIngressFromAWSLoadBalancerOptions{
				LoadBalancer: loadBalancer,
				ClusterName:  ac.clusterName,
				Recorder:     ac.recorder,
			})
			if err != nil {
				logger.Fatalf(err.Error())
			}

			ingresses = append(ingresses, albIngress)
		}(&wg, loadBalancer)
	}
	wg.Wait()

	ac.ALBIngresses = ingresses

	logger.Infof("Assembled %d ingresses from existing AWS resources", len(ac.ALBIngresses))
}

// GetNodes returns a list of the cluster node external ids
func (ac *ALBController) GetNodes() util.AWSStringSlice {
	var result util.AWSStringSlice
	nodes := ac.storeLister.Node.List()
	for _, node := range nodes {
		result = append(result, aws.String(node.(*api.Node).Spec.ExternalID))
	}
	sort.Sort(result)
	return result
}
