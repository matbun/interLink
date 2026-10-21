package virtualkubelet

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testTenantNamespace = "alice"

func tenantPod(namespace string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "train", Namespace: namespace, UID: "uid-1"}}
}

func TestCheckTenantNamespace(t *testing.T) {
	confined := &Provider{config: Config{TenantNamespace: testTenantNamespace}}
	open := &Provider{config: Config{}}

	assert.NoError(t, confined.checkTenantNamespace(tenantPod(testTenantNamespace)))
	assert.ErrorContains(t, confined.checkTenantNamespace(tenantPod("bob")), `serves only namespace "alice"`)
	assert.NoError(t, open.checkTenantNamespace(tenantPod("bob")))
}

// TestCreatePodRefusesOtherNamespace checks that the refusal comes before anything
// else: the fake API server records no action at all.
func TestCreatePodRefusesOtherNamespace(t *testing.T) {
	client := fake.NewSimpleClientset()
	p := &Provider{clientSet: client, config: normalized(t, func() Config {
		c := sshConfig()
		c.TenantNamespace = testTenantNamespace
		return c
	}())}

	err := p.CreatePod(t.Context(), tenantPod("bob"))

	assert.ErrorContains(t, err, "TenantNamespace")
	assert.Empty(t, client.Actions())
}

func TestShadowLivesInTenantNamespace(t *testing.T) {
	pod := tenantPod(testTenantNamespace)

	confined, err := (&Provider{config: Config{TenantNamespace: testTenantNamespace}}).shadowResourceIdentity(pod)
	require.NoError(t, err)
	open, err := (&Provider{config: Config{}}).shadowResourceIdentity(pod)
	require.NoError(t, err)

	assert.Equal(t, testTenantNamespace, confined.Namespace)
	assert.Equal(t, testTenantNamespace+shadowNamespaceSuffix, open.Namespace)
}

// TestShadowInPodNamespaceNeedsNoNamespaceAccess checks that a shadow in the pod's
// own namespace never reads or creates Namespace objects, which a virtual node with
// only namespaced RBAC may not do.
func TestShadowInPodNamespaceNeedsNoNamespaceAccess(t *testing.T) {
	client := fake.NewSimpleClientset()
	config := sshConfig()
	config.TenantNamespace = testTenantNamespace
	p := &Provider{clientSet: client, config: normalized(t, config)}

	// It fails later on the missing credential; only the calls made up to then matter.
	_, _, err := p.createShadowPod(t.Context(), tenantPod(testTenantNamespace))

	require.Error(t, err)
	t.Log(err)
	for _, a := range client.Actions() {
		assert.NotEqual(t, "namespaces", a.GetResource().Resource, "unexpected %s on namespaces", a.GetVerb())
	}
}

func TestTenantNamespaceDefaultsToNoReplication(t *testing.T) {
	config := sshConfig()
	config.Network.SSH.ReplicateCredentials = nil
	config.TenantNamespace = testTenantNamespace

	require.NoError(t, NormalizeShadowConfig(&config))

	assert.False(t, *config.Network.SSH.ReplicateCredentials)
}
