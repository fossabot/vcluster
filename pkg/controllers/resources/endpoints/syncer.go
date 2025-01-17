package endpoints

import (
	"context"
	"encoding/json"
	context2 "github.com/loft-sh/vcluster/cmd/vcluster/context"
	"github.com/loft-sh/vcluster/pkg/controllers/resources/generic"
	"github.com/loft-sh/vcluster/pkg/util/loghelper"
	"github.com/loft-sh/vcluster/pkg/util/translate"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

func RegisterIndices(ctx *context2.ControllerContext) error {
	return generic.RegisterSyncerIndices(ctx, &corev1.Endpoints{})
}

func Register(ctx *context2.ControllerContext) error {
	var err error
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubernetes.NewForConfigOrDie(ctx.VirtualManager.GetConfig()).CoreV1().Events("")})

	serviceClient := ctx.LocalManager.GetClient()
	if ctx.Options.ServiceNamespace != ctx.Options.TargetNamespace {
		serviceClient, err = client.New(ctx.LocalManager.GetConfig(), client.Options{
			Scheme: ctx.LocalManager.GetScheme(),
			Mapper: ctx.LocalManager.GetRESTMapper(),
		})
		if err != nil {
			return errors.Wrap(err, "create uncached client")
		}
	}

	return generic.RegisterSyncer(ctx, &syncer{
		eventRecoder:     eventBroadcaster.NewRecorder(ctx.VirtualManager.GetScheme(), corev1.EventSource{Component: "endpoints-syncer"}),
		targetNamespace:  ctx.Options.TargetNamespace,
		serviceName:      ctx.Options.ServiceName,
		serviceNamespace: ctx.Options.ServiceNamespace,
		serviceClient:    serviceClient,
		localClient:      ctx.LocalManager.GetClient(),
		virtualClient:    ctx.VirtualManager.GetClient(),
	}, "endpoints", generic.RegisterSyncerOptions{})
}

type syncer struct {
	eventRecoder    record.EventRecorder
	targetNamespace string

	serviceName      string
	serviceNamespace string
	serviceClient    client.Client

	localClient   client.Client
	virtualClient client.Client
}

func (s *syncer) New() client.Object {
	return &corev1.Endpoints{}
}

func (s *syncer) NewList() client.ObjectList {
	return &corev1.EndpointsList{}
}

func (s *syncer) ForwardCreate(ctx context.Context, vObj client.Object, log loghelper.Logger) (ctrl.Result, error) {
	vEndpoints := vObj.(*corev1.Endpoints)
	newEndpoints, err := s.translate(vObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	log.Infof("create physical endpoints %s/%s", newEndpoints.Namespace, newEndpoints.Name)
	err = s.localClient.Create(ctx, newEndpoints)
	if err != nil {
		log.Infof("error syncing %s/%s to physical cluster: %v", vEndpoints.Namespace, vEndpoints.Name, err)
		s.eventRecoder.Eventf(vEndpoints, "Warning", "SyncError", "Error syncing to physical cluster: %v", err)
		return ctrl.Result{RequeueAfter: time.Second}, err
	}

	return ctrl.Result{}, nil
}

func (s *syncer) ForwardCreateNeeded(vObj client.Object) (bool, error) {
	// dont do anything for the kubernetes endpoints
	vEndpoints := vObj.(*corev1.Endpoints)
	if vEndpoints.Name == "kubernetes" && vEndpoints.Namespace == "default" {
		return false, nil
	}

	return true, nil
}

func (s *syncer) ForwardUpdate(ctx context.Context, pObj client.Object, vObj client.Object, log loghelper.Logger) (ctrl.Result, error) {
	// did the endpoints change?
	pEndpoints := pObj.(*corev1.Endpoints)
	vEndpoints := vObj.(*corev1.Endpoints)
	updated, err := s.calcEndpointsDiff(pEndpoints, vEndpoints)
	if err != nil {
		return ctrl.Result{}, err
	}
	if updated != nil {
		log.Infof("updating physical endpoints %s/%s, because virtual endpoints have changed", updated.Namespace, updated.Name)
		err := s.localClient.Update(ctx, updated)
		if err != nil {
			s.eventRecoder.Eventf(vEndpoints, "Warning", "SyncError", "Error syncing to physical cluster: %v", err)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (s *syncer) ForwardUpdateNeeded(pObj client.Object, vObj client.Object) (bool, error) {
	updated, err := s.calcEndpointsDiff(pObj.(*corev1.Endpoints), vObj.(*corev1.Endpoints))
	return updated != nil, err
}

func (s *syncer) translate(vObj runtime.Object) (*corev1.Endpoints, error) {
	newObj, err := translate.SetupMetadata(s.targetNamespace, vObj)
	if err != nil {
		return nil, errors.Wrap(err, "error setting metadata")
	}

	// translate the addresses
	endpoints := newObj.(*corev1.Endpoints)
	for i, subset := range endpoints.Subsets {
		for j, addr := range subset.Addresses {
			if addr.TargetRef != nil && addr.TargetRef.Kind == "Pod" {
				endpoints.Subsets[i].Addresses[j].TargetRef.Name = translate.PhysicalName(addr.TargetRef.Name, addr.TargetRef.Namespace)
				endpoints.Subsets[i].Addresses[j].TargetRef.Namespace = s.targetNamespace

				// TODO: set the actual values here
				endpoints.Subsets[i].Addresses[j].TargetRef.UID = ""
				endpoints.Subsets[i].Addresses[j].TargetRef.ResourceVersion = ""
			}
		}
		for j, addr := range subset.NotReadyAddresses {
			if addr.TargetRef != nil && addr.TargetRef.Kind == "Pod" {
				endpoints.Subsets[i].NotReadyAddresses[j].TargetRef.Name = translate.PhysicalName(addr.TargetRef.Name, addr.TargetRef.Namespace)
				endpoints.Subsets[i].NotReadyAddresses[j].TargetRef.Namespace = s.targetNamespace

				// TODO: set the actual values here
				endpoints.Subsets[i].NotReadyAddresses[j].TargetRef.UID = ""
				endpoints.Subsets[i].NotReadyAddresses[j].TargetRef.ResourceVersion = ""
			}
		}
	}

	// make sure we delete the control-plane.alpha.kubernetes.io/leader annotation
	// that will disable endpoint slice mirroring otherwise
	if endpoints.Annotations != nil {
		delete(endpoints.Annotations, "control-plane.alpha.kubernetes.io/leader")
	}

	return endpoints, nil
}

func (s *syncer) calcEndpointsDiff(pObj, vObj *corev1.Endpoints) (*corev1.Endpoints, error) {
	var updated *corev1.Endpoints

	// translate endpoints
	translated, err := s.translate(vObj)
	if err != nil {
		return nil, err
	}

	// check subsets
	if !equality.Semantic.DeepEqual(translated.Subsets, pObj.Subsets) {
		updated = pObj.DeepCopy()
		updated.Subsets = translated.Subsets
	}

	// check annotations
	if !equality.Semantic.DeepEqual(translated.Annotations, pObj.Annotations) {
		if updated == nil {
			updated = pObj.DeepCopy()
		}
		updated.Annotations = translated.Annotations
	}

	// check labels
	if !equality.Semantic.DeepEqual(translated.Labels, pObj.Labels) {
		if updated == nil {
			updated = pObj.DeepCopy()
		}

		updated.Labels = translated.Labels
	}

	return updated, nil
}

func (s *syncer) BackwardUpdate(ctx context.Context, pObj client.Object, vObj client.Object, log loghelper.Logger) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

func (s *syncer) BackwardUpdateNeeded(pObj client.Object, vObj client.Object) (bool, error) {
	return false, nil
}

func (s *syncer) BackwardStart(ctx context.Context, req ctrl.Request) (bool, error) {
	// sync the kubernetes service
	if req.Name == s.serviceName && req.Namespace == s.serviceNamespace {
		return true, SyncKubernetesServiceEndpoints(ctx, s.virtualClient, s.serviceClient, s.serviceNamespace, s.serviceName)
	}

	return false, nil
}

func (s *syncer) BackwardEnd() {

}

func (s *syncer) ForwardStart(ctx context.Context, req ctrl.Request) (bool, error) {
	// dont do anything for the kubernetes service
	if req.Name == "kubernetes" && req.Namespace == "default" {
		return true, SyncKubernetesServiceEndpoints(ctx, s.virtualClient, s.serviceClient, s.serviceNamespace, s.serviceName)
	}

	return false, nil
}

func (s *syncer) ForwardEnd() {

}

func SyncKubernetesServiceEndpoints(ctx context.Context, virtualClient client.Client, localClient client.Client, serviceNamespace, serviceName string) error {
	// get physical service endpoints
	pObj := &corev1.Endpoints{}
	err := localClient.Get(ctx, types.NamespacedName{
		Namespace: serviceNamespace,
		Name:      serviceName,
	}, pObj)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	// get virtual service endpoints
	vObj := &corev1.Endpoints{}
	err = virtualClient.Get(ctx, types.NamespacedName{
		Namespace: "default",
		Name:      "kubernetes",
	}, vObj)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	// build new subsets
	newSubsets := pObj.DeepCopy().Subsets
	for i := range newSubsets {
		for j := range newSubsets[i].Ports {
			newSubsets[i].Ports[j].Name = "https"
		}
		for j := range pObj.Subsets[i].Addresses {
			newSubsets[i].Addresses[j].Hostname = ""
			newSubsets[i].Addresses[j].NodeName = nil
			newSubsets[i].Addresses[j].TargetRef = nil
		}
		for j := range pObj.Subsets[i].NotReadyAddresses {
			newSubsets[i].NotReadyAddresses[j].Hostname = ""
			newSubsets[i].NotReadyAddresses[j].NodeName = nil
			newSubsets[i].NotReadyAddresses[j].TargetRef = nil
		}
	}

	oldJSON, err := json.Marshal(vObj.Subsets)
	if err != nil {
		return err
	}
	newJSON, err := json.Marshal(newSubsets)
	if err != nil {
		return err
	}

	if string(oldJSON) == string(newJSON) {
		return nil
	}

	vObj.Subsets = newSubsets
	return virtualClient.Update(ctx, vObj)
}
