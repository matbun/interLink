package virtualkubelet

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
)

// checkTenantNamespace refuses a pod from outside the one namespace a confined virtual
// node serves (Config.TenantNamespace). The virtual kubelet does not even watch other
// namespaces then, so this only matters for a pod bound to the node by hand.
func (p *Provider) checkTenantNamespace(pod *v1.Pod) error {
	if p.config.TenantNamespace == "" || pod.Namespace == p.config.TenantNamespace {
		return nil
	}
	return fmt.Errorf("pod %s/%s refused: this virtual node serves only namespace %q (TenantNamespace)",
		pod.Namespace, pod.Name, p.config.TenantNamespace)
}
