package virtualkubelet

import v1 "k8s.io/api/core/v1"

// computeShadowResourceIdentity is the identity of a pod's shadow on a node not confined
// to one namespace. It exists in the tests only: production code must use
// p.shadowResourceIdentity, which honours TenantNamespace, and a port that brings back a
// call to this function no longer compiles.
func computeShadowResourceIdentity(pod *v1.Pod) (shadowResourceIdentity, error) {
	return shadowResourceIdentityFor(pod, isShadowSameNamespace(pod))
}
