package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/golang/glog"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"k8s.io/canary-deploy-controller/pkg/apis/canarydeploycontroller/v1"
	clientset "k8s.io/canary-deploy-controller/pkg/client/clientset/versioned"
	sscheme "k8s.io/canary-deploy-controller/pkg/client/clientset/versioned/scheme"
	informers "k8s.io/canary-deploy-controller/pkg/client/informers/externalversions"
	listers "k8s.io/canary-deploy-controller/pkg/client/listers/canarydeploycontroller/v1"
)

const controllerAgentName = "canarydeploy_controller"

// Controller is the controller implementation for Canary resources
type Controller struct {
	kubeclientset kubernetes.Interface
	cdclientset   clientset.Interface

	cdLister listers.CanaryLister
	cdSynced cache.InformerSynced

	deploymentsLister appslisters.DeploymentLister
	deploymentsSynced cache.InformerSynced

	servicesLister corelisters.ServiceLister
	servicesSynced cache.InformerSynced

	queue    workqueue.RateLimitingInterface
	recorder record.EventRecorder
}

// NewController returns a new instance of a controller
func NewController(
	kubeclientset kubernetes.Interface,
	cdclientset clientset.Interface,

	kubeInformerFactory kubeinformers.SharedInformerFactory,
	cdInformerFactory informers.SharedInformerFactory) *Controller {

	cdInformer := cdInformerFactory.Canarydeploycontroller().V1().Canaries()
	sscheme.AddToScheme(scheme.Scheme)

	deploymentInformer := kubeInformerFactory.Apps().V1().Deployments()
	serviceInformer := kubeInformerFactory.Core().V1().Services()

	glog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	c := &Controller{
		kubeclientset: kubeclientset,
		cdclientset:   cdclientset,

		cdLister: cdInformer.Lister(),
		cdSynced: cdInformer.Informer().HasSynced,

		deploymentsLister: deploymentInformer.Lister(),
		deploymentsSynced: deploymentInformer.Informer().HasSynced,

		servicesLister: serviceInformer.Lister(),
		servicesSynced: serviceInformer.Informer().HasSynced,

		queue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Canaries"),
		recorder: recorder,
	}

	glog.Info("Setting up event handlers")
	// Set up an event handler for when EventProvider resources change
	cdInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// glog.Info("AddFunc called with object: %v", obj)
			// key, err := cache.MetaNamespaceKeyFunc(obj)
			// if err == nil {
			// 	c.queue.Add(key)
			// }
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			glog.Info("UpdateFunc called with objects: %v, %v", old, new)
			oldCrd := old.(*v1.Canary)
			newCrd := new.(*v1.Canary)

			if oldCrd.ResourceVersion == newCrd.ResourceVersion {
				// Periodic resync will send update events for all known Objects.
				// Two different versions of the same Objects will always have different RVs.
				return
			}

			if reflect.DeepEqual(oldCrd.Spec, newCrd.Spec) {
				return
			}

			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				c.queue.Add(key)
			}

		},
		DeleteFunc: func(obj interface{}) {
			// glog.Info("DeleteFunc called with object: %v", obj)
			// // IndexerInformer uses a delta nodeQueue, therefore for deletes we have to use this
			// // key function.
			// key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			// if err == nil {
			// 	c.queue.Add(key)
			// }
		},
	})

	return c
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.queue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	glog.Info("Starting Canary Deploy controller")

	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.cdSynced, c.deploymentsSynced, c.servicesSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	glog.Info("Starting workers")
	// Launch two workers to process the resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, 30*time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.queue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.queue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.queue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Foo resource to be synced.
		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.queue.Forget(obj)
		glog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two
func (c *Controller) syncHandler(key string) error {

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}
	glog.Infof("\nReceived: namespace: %v, name: %v\n", namespace, name)

	cd, err := c.cdLister.Canaries(namespace).Get(name)
	if err != nil {
		return fmt.Errorf("error getting resource: %v", err)
	}
	glog.Infof("canarydeploy: %v", cd)

	switch cd.Spec.Operation {
	case "Do":
		if cd.Status.DeployStatus != 0 {
			return fmt.Errorf("can not operate canary do in canary %s, because haved a canary do", cd.Spec.ServiceName)
		}
		// get canary deployment to
		deploy, deployNamespace, deploymentName, err := newCanaryDeployment(cd)
		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		deployment, err := c.deploymentsLister.Deployments(deployNamespace).Get(deploy.Name)
		// If the resource doesn't exist, we'll create it
		if errors.IsNotFound(err) {
			deployment, err = c.kubeclientset.AppsV1().Deployments(deployNamespace).Create(deploy)
		}
		glog.Infof("create deployment name: %v in canary do", deployment.Name)

		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		// get service to deploy
		service, err := c.servicesLister.Services(deployNamespace).Get(cd.Spec.ServiceName)
		if errors.IsNotFound(err) {
			glog.Infof("service name: %v is not exist, please check config of crd", cd.Spec.ServiceName)
			return err
		}

		// new release service
		releaseServiceName := fmt.Sprintf("%s-release", cd.Spec.ServiceName)
		releaseService, err := c.servicesLister.Services(deployNamespace).Get(releaseServiceName)
		// If the resource doesn't exist, we'll create it
		if errors.IsNotFound(err) {
			//
			releaseService, err = c.kubeclientset.CoreV1().Services(deployNamespace).Create(newReleaseServiceFromService(cd, service, deploymentName))
		}
		glog.Infof("create service name: %v in canary do", releaseService.Name)
		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		// new canary service
		canaryServiceName := fmt.Sprintf("%s-canary", cd.Spec.ServiceName)
		canaryService, err := c.servicesLister.Services(deployNamespace).Get(canaryServiceName)
		// If the resource doesn't exist, we'll create it
		if errors.IsNotFound(err) {
			//
			canaryService, err = c.kubeclientset.CoreV1().Services(deployNamespace).Create(newCanaryServiceFromService(cd, service, deploymentName))
		}
		glog.Infof("create service name: %v in canary do", canaryService.Name)
		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		// create ambassdor
		// create ambassador deployment
		ambassadorDeploymentName := fmt.Sprintf("%s-proxy", deploymentName)
		_, err = c.deploymentsLister.Deployments(deployNamespace).Get(ambassadorDeploymentName)
		// If the resource doesn't exist, we'll create it
		if errors.IsNotFound(err) {
			_, err = c.kubeclientset.AppsV1().Deployments(deployNamespace).Create(newAmbassadorDeployment(service, deploymentName))
		}
		glog.Infof("create deployment name: %v in canary do", ambassadorDeploymentName)

		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		// Finally, we update the status block of the Employee resource to reflect the
		// current state of the world
		err = c.updateCanaryDeployStatus(cd, deployNamespace, 1)
		if err != nil {
			return err
		}

	case "Adopt":
		if cd.Status.DeployStatus != 1 {
			return fmt.Errorf("can not operate canary adopt in canary %s, adopt must before do", cd.Spec.ServiceName)
		}
		// get canary deployment to
		deploy, deployNamespace, deploymentName, err := newCanaryDeployment(cd)
		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		policy := metav1.DeletePropagationBackground
		deleteOptions := &metav1.DeleteOptions{
			PropagationPolicy: &policy,
		}

		ambassadorDeploymentName := fmt.Sprintf("%s-proxy", deploymentName)
		_, err = c.deploymentsLister.Deployments(deployNamespace).Get(ambassadorDeploymentName)
		// If the resource doesn't exist, we'll create it
		if err == nil {
			err = c.kubeclientset.AppsV1().Deployments(deployNamespace).Delete(ambassadorDeploymentName, deleteOptions)
		}
		glog.Infof("delete deployment name: %v in canary adopt", ambassadorDeploymentName)

		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		// delete release service
		releaseServiceName := fmt.Sprintf("%s-release", cd.Spec.ServiceName)
		_, err = c.servicesLister.Services(deployNamespace).Get(releaseServiceName)
		// If the resource  exist, we'll delete it
		if err == nil {
			//
			err = c.kubeclientset.CoreV1().Services(deployNamespace).Delete(releaseServiceName, deleteOptions)
		}
		glog.Infof("delete service name: %v in canary adopt", releaseServiceName)
		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		// delete canary service
		canaryServiceName := fmt.Sprintf("%s-canary", cd.Spec.ServiceName)
		_, err = c.servicesLister.Services(deployNamespace).Get(canaryServiceName)
		// If the resource  exist, we'll delete it
		if err == nil {
			//
			err = c.kubeclientset.CoreV1().Services(deployNamespace).Delete(canaryServiceName, deleteOptions)
		}
		glog.Infof("delete service name: %v in canary adopt", canaryServiceName)
		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		_, err = c.deploymentsLister.Deployments(deployNamespace).Get(deploy.Name)
		// If the resource doesn't exist, we'll create it
		if err == nil {
			err = c.kubeclientset.AppsV1().Deployments(deployNamespace).Delete(deploy.Name, deleteOptions)
		}
		glog.Infof("delete deployment name: %v in canary adopt", deploymentName+"-canary")

		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		deployment, err := c.deploymentsLister.Deployments(deployNamespace).Get(deploymentName)
		// If the resource doesn't exist, we'll create it
		if err == nil {
			releaseDeployment, err := newDeploymentFromYaml(cd.Spec.DeployYaml)
			if err == nil {
				_, err = c.kubeclientset.AppsV1().Deployments(deployNamespace).Update(releaseDeployment)
				if err != nil {
					glog.Info(err.Error())
					return err
				}
				fmt.Printf("update deployment name: %v in canary adopt", deployment.Name)
			}

		}

		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		// Finally, we update the status block of the Employee resource to reflect the
		// current state of the world
		err = c.updateCanaryDeployStatus(cd, deployNamespace, 0)
		if err != nil {
			return err
		}

	case "Deprecate":
		if cd.Status.DeployStatus != 1 {
			return fmt.Errorf("can not operate canary deprecate in canary %s, deprecate must before do", cd.Spec.ServiceName)
		}
		// get canary deployment to
		deploy, deployNamespace, deploymentName, err := newCanaryDeployment(cd)
		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		policy := metav1.DeletePropagationBackground
		deleteOptions := &metav1.DeleteOptions{
			PropagationPolicy: &policy,
		}

		ambassadorDeploymentName := fmt.Sprintf("%s-proxy", deploymentName)
		_, err = c.deploymentsLister.Deployments(deployNamespace).Get(ambassadorDeploymentName)
		// If the resource doesn't exist, we'll create it
		if err == nil {
			err = c.kubeclientset.AppsV1().Deployments(deployNamespace).Delete(ambassadorDeploymentName, deleteOptions)
		}
		glog.Infof("delete deployment name: %v in canary adopt", ambassadorDeploymentName)

		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		// delete release service
		releaseServiceName := fmt.Sprintf("%s-release", cd.Spec.ServiceName)
		_, err = c.servicesLister.Services(deployNamespace).Get(releaseServiceName)
		// If the resource doesn't exist, we'll create it
		if err == nil {
			//
			err = c.kubeclientset.CoreV1().Services(deployNamespace).Delete(releaseServiceName, deleteOptions)
		}
		glog.Infof("delete service name: %v in canary back", releaseServiceName)
		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		// delete canary service
		canaryServiceName := fmt.Sprintf("%s-canary", cd.Spec.ServiceName)
		_, err = c.servicesLister.Services(deployNamespace).Get(canaryServiceName)
		// If the resource doesn't exist, we'll create it
		if err == nil {
			//
			err = c.kubeclientset.CoreV1().Services(deployNamespace).Delete(canaryServiceName, deleteOptions)
		}
		glog.Infof("delete service name: %v in canary back", canaryServiceName)
		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		_, err = c.deploymentsLister.Deployments(deployNamespace).Get(deploy.Name)
		// If the resource doesn't exist, we'll create it
		if err == nil {
			err = c.kubeclientset.AppsV1().Deployments(deployNamespace).Delete(deploy.Name, deleteOptions)
		}
		glog.Infof("delete deployment name: %v in canary back", deploy.Name)

		// If an error occurs during Get/Create, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil {
			glog.Info(err.Error())
			return err
		}

		// Finally, we update the status block of the Employee resource to reflect the
		// current state of the world
		err = c.updateCanaryDeployStatus(cd, deployNamespace, 0)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("cannot handle operation %v", cd.Spec.Operation)
	}
	return nil
}

func newReleaseServiceFromService(canary *v1.Canary, dest *corev1.Service, deploymentName string) *corev1.Service {
	copy := dest.DeepCopy()

	copy.ObjectMeta.Annotations["getambassador.io/config"] = fmt.Sprintf(releaseAmbassador, dest.Name, canary.Spec.Prefix, dest.Name, dest.Namespace)
	copy.Spec.Selector = map[string]string{
		"trace": fmt.Sprintf("%s-release", deploymentName),
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        copy.ObjectMeta.Name + "-release",
			Namespace:   copy.ObjectMeta.Namespace,
			Annotations: copy.ObjectMeta.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Ports:           copy.Spec.Ports,
			Selector:        copy.Spec.Selector,
			SessionAffinity: copy.Spec.SessionAffinity,
		},
	}
}

func newCanaryServiceFromService(canary *v1.Canary, dest *corev1.Service, deploymentName string) *corev1.Service {
	copy := dest.DeepCopy()
	copy.Spec.Selector["trace"] = deploymentName + "-canary"

	copy.ObjectMeta.Annotations["getambassador.io/config"] = fmt.Sprintf(canaryAmbassador, dest.Name, canary.Spec.Prefix, dest.Name, dest.Namespace, canary.Spec.Weight)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        copy.ObjectMeta.Name + "-canary",
			Namespace:   copy.ObjectMeta.Namespace,
			Annotations: copy.ObjectMeta.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Ports:    copy.Spec.Ports,
			Selector: copy.Spec.Selector,
		},
	}

}

const releaseAmbassador = `---
apiVersion: ambassador/v0
kind: Mapping
name: %s-release-mapping
prefix: %s
service: %s-release.%s
`
const canaryAmbassador = `---
apiVersion: ambassador/v0
kind: Mapping
name: %s-canary-mapping
prefix: %s
service: %s-canary.%s
weight: %d
`

func newDeploymentFromYaml(content string) (*appsv1.Deployment, error) {
	bytes, err := yaml.ToJSON([]byte(content))
	if err != nil {
		return nil, err
	}

	var deploy appsv1.Deployment
	err = json.Unmarshal(bytes, &deploy)
	if err != nil {
		fmt.Println(err.Error())
		return nil, err
	}

	return &deploy, nil
}

func newCanaryDeployment(canary *v1.Canary) (*appsv1.Deployment, string, string, error) {
	deploy, err := newDeploymentFromYaml(canary.Spec.DeployYaml)
	if err != nil {
		fmt.Println(err.Error())
		return nil, "", "", err
	}
	preName := deploy.Name
	deploy.SetName(preName + "-canary")

	var replicas int32
	replicas = 1
	deploy.Spec.Replicas = &replicas
	deploy.Spec.Template.Labels["trace"] = preName + "-canary"

	return deploy, deploy.Namespace, preName, nil
}

func newAmbassadorDeployment(dest *corev1.Service, deploymentName string) *appsv1.Deployment {
	var replicas int32
	replicas = 1

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName + "-proxy",
			Namespace: dest.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: dest.Spec.Selector,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: dest.Spec.Selector,
					Annotations: map[string]string{
						"sidecar.istio.io/inject": "false",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "ambassador",
					Containers: []corev1.Container{
						{
							Name:  "ambassador",
							Image: "quay.io/datawire/ambassador:0.33.0",
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									Protocol:      corev1.ProtocolTCP,
									ContainerPort: dest.Spec.Ports[0].TargetPort.IntVal,
								},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("400Mi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
							Env: []corev1.EnvVar{
								{
									Name: "AMBASSADOR_NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.namespace",
										},
									},
								},
								{
									Name:  "AMBASSADOR_LISTEN_PORT",
									Value: dest.Spec.Ports[0].TargetPort.StrVal,
								},
							},
							LivenessProbe: &corev1.Probe{
								Handler: corev1.Handler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/ambassador/v0/check_alive",
										Port: intstr.FromInt(8877),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       3,
							},
							ReadinessProbe: &corev1.Probe{
								Handler: corev1.Handler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/ambassador/v0/check_ready",
										Port: intstr.FromInt(8877),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       3,
							},
						},
					},
				},
			},
		},
	}
}

func (c *Controller) updateCanaryDeployStatus(cd *v1.Canary, deployNamespace string, status int32) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	cdCopy := cd.DeepCopy()
	cdCopy.Status.DeployStatus = status

	// Until #38113 is merged, we must use Update instead of UpdateStatus to
	// update the Status block of the Employee resource. UpdateStatus will not
	// allow changes to the Spec of the resource, which is ideal for ensuring
	// nothing other than resource status has been updated.
	_, err := c.cdclientset.CanarydeploycontrollerV1().Canaries(deployNamespace).Update(cdCopy)
	return err
}
