package virtualkubelet

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// tenantRefusedReason is the pod status reason of a pod a confined node refused.
const tenantRefusedReason = "TenantNamespace"

// checkTenantNamespace refuses a pod from outside the one namespace a confined virtual
// node serves (Config.TenantNamespace). The pod controller only watches that namespace,
// so this guards the create path; RefuseForeignPods fails the pods it never sees.
func (p *Provider) checkTenantNamespace(pod *v1.Pod) error {
	return checkTenantNamespace(p.config.TenantNamespace, pod)
}

func checkTenantNamespace(tenant string, pod *v1.Pod) error {
	if tenant == "" || pod.Namespace == tenant {
		return nil
	}
	return fmt.Errorf("pod %s/%s refused: this virtual node serves only namespace %q (TenantNamespace)",
		pod.Namespace, pod.Name, tenant)
}

// refuseForeignPod marks a pod bound to a confined node from another namespace as
// Failed, as the kubelet does with a pod it rejects at admission, instead of leaving
// it Pending forever (e.g. a DaemonSet pod that tolerates every taint).
func refuseForeignPod(ctx context.Context, client kubernetes.Interface, tenant string, pod *v1.Pod) error {
	refusal := checkTenantNamespace(tenant, pod)
	if refusal == nil || pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded {
		return nil
	}
	failed := pod.DeepCopy()
	failed.Status.Phase = v1.PodFailed
	failed.Status.Reason = tenantRefusedReason
	failed.Status.Message = refusal.Error()
	_, err := client.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, failed, metav1.UpdateOptions{})
	if err == nil {
		log.G(ctx).Warningf("Failed pod %s/%s bound to this node: %v", pod.Namespace, pod.Name, refusal)
	}
	return err
}

// RefuseForeignPods watches the pods bound to nodeName in every namespace and fails
// those outside tenant. It needs list/watch on pods and update on pods/status across
// the cluster; without them it logs once and stops, and such pods stay Pending.
func RefuseForeignPods(ctx context.Context, client kubernetes.Interface, nodeName, tenant string) {
	if tenant == "" {
		return
	}
	factory := informers.NewSharedInformerFactoryWithOptions(client, 5*time.Minute,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = "spec.nodeName=" + nodeName
		}))
	informer := factory.Core().V1().Pods().Informer()
	forbidden := false
	if err := informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		if apierrors.IsForbidden(err) && !forbidden {
			forbidden = true
			log.G(ctx).Warningf("Cannot watch pods outside namespace %q, so pods bound to this node from other namespaces stay Pending: %v", tenant, err)
		}
	}); err != nil {
		log.G(ctx).Warningf("Cannot set the error handler of the watch on pods bound to this node: %v", err)
	}
	handle := func(obj any) {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			return
		}
		if err := refuseForeignPod(ctx, client, tenant, pod); err != nil && !apierrors.IsNotFound(err) {
			log.G(ctx).Warningf("Could not fail refused pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
	}
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    handle,
		UpdateFunc: func(_, obj any) { handle(obj) },
	}); err != nil {
		log.G(ctx).Warningf("Cannot refuse pods bound to this node from other namespaces: %v", err)
		return
	}
	go informer.Run(ctx.Done())
}
