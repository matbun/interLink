package virtualkubelet

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

const keptInitName = "stage-in"

// podKeepingInit is an offloaded pod whose first init container stays on the cluster.
func podKeepingInit(policy v1.RestartPolicy) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "trainer", Namespace: testNamespaceDefault, UID: k8stypes.UID("uid-kept"),
			Annotations: map[string]string{annDisableOffloadInitContainers: keptInitName},
		},
		Spec: v1.PodSpec{
			RestartPolicy: policy,
			InitContainers: []v1.Container{
				{Name: keptInitName, Image: "rsync"},
				{Name: "warmup", Image: "app"},
			},
			Containers: []v1.Container{{Name: testAppName, Image: "app"}},
		},
	}
}

// shadowOf returns the shadow pod of pod with the given status for the kept init container.
func shadowOf(t *testing.T, pod *v1.Pod, status v1.ContainerStatus) *v1.Pod {
	t.Helper()
	identity, err := computeShadowResourceIdentity(pod)
	require.NoError(t, err)
	status.Name = keptInitName
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: identity.Name + "-abc", Namespace: identity.Namespace,
			Labels: map[string]string{shadowComponentLabel: identity.Name},
		},
		Status: v1.PodStatus{InitContainerStatuses: []v1.ContainerStatus{status}},
	}
}

func exited(code int32) v1.ContainerStatus {
	return v1.ContainerStatus{State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{ExitCode: code}}}
}

func keptProvider(client *fake.Clientset, pod *v1.Pod) *Provider {
	return &Provider{
		clientSet: client,
		notifier:  func(*v1.Pod) {},
		pods:      map[string]*v1.Pod{string(pod.UID): pod},
	}
}

func TestHasShadowForKeptContainers(t *testing.T) {
	p := &Provider{config: Config{Network: Network{EnableTunnel: false}}}
	pod := podKeepingInit(v1.RestartPolicyNever)

	assert.True(t, p.hasShadow(pod), "kept containers only run in a shadow, tunnel or not")
	assert.False(t, p.hasTunnelShadow(pod))

	delete(pod.Annotations, annDisableOffloadInitContainers)
	assert.False(t, p.hasShadow(pod), "nothing kept, no ports: no shadow")
}

func TestKeptShadowTemplate(t *testing.T) {
	// A custom tunnel template must not be used for a shadow that has no tunnel.
	p := &Provider{config: Config{Network: Network{WstunnelTemplatePath: "/nonexistent/custom.yaml"}}}
	out, err := p.executeShadowTemplate(t.Context(), ShadowTemplateData{
		Name: "shadow-trainer", Namespace: testNamespaceDefault, KeptOnly: true, PauseImage: p.shadowPauseImage(),
	})
	require.NoError(t, err)
	assert.NotContains(t, out, "kind: Service")

	var obj map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(out), &obj))
	assert.Equal(t, "Deployment", obj["kind"])
	assert.Contains(t, out, "image: "+defaultShadowPauseImage)
	assert.Contains(t, out, "kubernetes.io/os: linux", "a shadow must never land on the virtual node")
}

func TestKeptInitProgress(t *testing.T) {
	kept := []v1.Container{{Name: keptInitName}}
	running := v1.ContainerStatus{State: v1.ContainerState{Running: &v1.ContainerStateRunning{}}}
	retrying := v1.ContainerStatus{
		RestartCount:         1,
		State:                v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{ExitCode: 3}},
	}
	pod := podKeepingInit(v1.RestartPolicyNever)

	cases := []struct {
		name       string
		status     *v1.ContainerStatus
		done       bool
		failedCode bool
	}{
		{"not started", nil, false, false},
		{"running", &running, false, false},
		{"completed", ptrTo(exited(0)), true, false},
		{"exited non-zero", ptrTo(exited(2)), false, true},
		{"failed and being retried", &retrying, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			shadow := &v1.Pod{}
			if c.status != nil {
				shadow = shadowOf(t, pod, *c.status)
			}
			state := keptInitProgress(shadow, kept)
			assert.Equal(t, c.done, state.done)
			assert.Equal(t, c.failedCode, state.failed != nil)
		})
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestWaitForKeptInitContainers(t *testing.T) {
	keptInitPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { keptInitPollInterval = 2 * time.Second })

	t.Run("submits once the kept init container completed, with its status mirrored", func(t *testing.T) {
		pod := podKeepingInit(v1.RestartPolicyNever)
		client := fake.NewSimpleClientset(shadowOf(t, pod, exited(0)))
		p := keptProvider(client, pod)

		require.NoError(t, p.waitForKeptInitContainers(t.Context(), pod))
		require.Len(t, pod.Status.InitContainerStatuses, 1)
		assert.Equal(t, keptInitName, pod.Status.InitContainerStatuses[0].Name)
	})

	t.Run("on a node confined to one namespace the shadow is in the pod's namespace", func(t *testing.T) {
		pod := podKeepingInit(v1.RestartPolicyNever)
		identity, err := shadowResourceIdentityFor(pod, true)
		require.NoError(t, err)
		shadow := shadowOf(t, pod, exited(0))
		shadow.Name, shadow.Namespace = identity.Name+"-abc", identity.Namespace
		shadow.Labels[shadowComponentLabel] = identity.Name
		p := keptProvider(fake.NewSimpleClientset(shadow), pod)
		p.config.TenantNamespace = pod.Namespace

		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		require.NoError(t, p.waitForKeptInitContainers(ctx, pod))
	})

	t.Run("a failure ends a restartPolicy Never pod without submitting", func(t *testing.T) {
		pod := podKeepingInit(v1.RestartPolicyNever)
		client := fake.NewSimpleClientset(shadowOf(t, pod, exited(2)))
		p := keptProvider(client, pod)

		require.Error(t, p.waitForKeptInitContainers(t.Context(), pod))
		assert.Equal(t, v1.PodFailed, pod.Status.Phase)
		assert.Equal(t, "InitContainerFailed", pod.Status.Reason)
	})

	t.Run("a failure is retried for restartPolicy OnFailure until it succeeds", func(t *testing.T) {
		pod := podKeepingInit(v1.RestartPolicyOnFailure)
		shadow := shadowOf(t, pod, exited(2))
		client := fake.NewSimpleClientset(shadow)
		p := keptProvider(client, pod)

		done := make(chan error, 1)
		go func() { done <- p.waitForKeptInitContainers(t.Context(), pod) }()

		select {
		case err := <-done:
			t.Fatalf("returned while the init container was failing: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		assert.NotEqual(t, v1.PodFailed, pod.Status.Phase)

		shadow.Status.InitContainerStatuses[0] = exited(0)
		shadow.Status.InitContainerStatuses[0].Name = keptInitName
		_, err := client.CoreV1().Pods(shadow.Namespace).UpdateStatus(t.Context(), shadow, metav1.UpdateOptions{})
		require.NoError(t, err)

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("still waiting after the init container succeeded")
		}
	})

	t.Run("stops when the pod is deleted while waiting", func(t *testing.T) {
		pod := podKeepingInit(v1.RestartPolicyNever)
		client := fake.NewSimpleClientset(shadowOf(t, pod, v1.ContainerStatus{
			State: v1.ContainerState{Running: &v1.ContainerStateRunning{}},
		}))
		p := keptProvider(client, pod)

		done := make(chan error, 1)
		go func() { done <- p.waitForKeptInitContainers(t.Context(), pod) }()
		time.Sleep(50 * time.Millisecond)
		p.podsMu.Lock()
		delete(p.pods, string(pod.UID))
		p.podsMu.Unlock()

		select {
		case err := <-done:
			assert.ErrorIs(t, err, errPodGoneWhileInitializing)
		case <-time.After(2 * time.Second):
			t.Fatal("still waiting for a deleted pod")
		}
	})

	t.Run("no kept init container: nothing to wait for", func(t *testing.T) {
		pod := podKeepingInit(v1.RestartPolicyNever)
		delete(pod.Annotations, annDisableOffloadInitContainers)
		p := keptProvider(fake.NewSimpleClientset(), pod)
		require.NoError(t, p.waitForKeptInitContainers(t.Context(), pod))
	})
}

func TestDropKeptOnlyVolumes(t *testing.T) {
	const dataVolume = "data"
	pod := podKeepingInit(v1.RestartPolicyNever)
	pod.Spec.InitContainers[0].VolumeMounts = []v1.VolumeMount{{Name: "credentials"}, {Name: dataVolume}}
	pod.Spec.Containers[0].VolumeMounts = []v1.VolumeMount{{Name: dataVolume}}
	pod.Spec.Volumes = []v1.Volume{{Name: "credentials"}, {Name: dataVolume}, {Name: "unused"}}

	offloaded := pod.DeepCopy()
	offloaded.Spec.Containers = getOffloadContainers(pod)
	offloaded.Spec.InitContainers = getOffloadInitContainers(pod)

	var names []string
	for _, v := range dropKeptOnlyVolumes(pod, offloaded) {
		names = append(names, v.Name)
	}
	assert.Equal(t, []string{dataVolume, "unused"}, names,
		"a volume only the kept init container mounts stays on the cluster; shared and unmounted ones are sent as before")
}

func TestKeptContainerLogs(t *testing.T) {
	keptInitPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { keptInitPollInterval = 2 * time.Second })

	read := func(t *testing.T, p *Provider, pod *v1.Pod) string {
		t.Helper()
		rc, err := p.GetLogs(t.Context(), pod.Namespace, pod.Name, keptInitName, api.ContainerLogOpts{})
		require.NoError(t, err)
		defer rc.Close()
		out, err := io.ReadAll(rc)
		require.NoError(t, err)
		return string(out)
	}

	t.Run("read from the shadow while it runs, kept once it ended", func(t *testing.T) {
		pod := podKeepingInit(v1.RestartPolicyNever)
		shadow := shadowOf(t, pod, exited(0))
		client := fake.NewSimpleClientset(pod, shadow)
		p := keptProvider(client, pod)

		assert.Equal(t, "fake logs", read(t, p, pod), "the fake clientset answers every log request so")

		require.NoError(t, p.waitForKeptInitContainers(t.Context(), pod))
		require.NoError(t, client.CoreV1().Pods(shadow.Namespace).Delete(t.Context(), shadow.Name, metav1.DeleteOptions{}))
		assert.Equal(t, "fake logs", read(t, p, pod), "the shadow is gone, the saved log remains")
	})

	t.Run("no shadow and nothing saved", func(t *testing.T) {
		pod := podKeepingInit(v1.RestartPolicyNever)
		p := keptProvider(fake.NewSimpleClientset(pod), pod)
		_, err := p.GetLogs(t.Context(), pod.Namespace, pod.Name, keptInitName, api.ContainerLogOpts{})
		require.ErrorContains(t, err, "no longer exists")
	})

	t.Run("options reach the shadow's log request", func(t *testing.T) {
		since := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
		o := shadowLogOptions(keptInitName, api.ContainerLogOpts{Tail: 5, LimitBytes: 100, SinceSeconds: 60, SinceTime: since, Follow: true})
		assert.Equal(t, keptInitName, o.Container)
		assert.Equal(t, int64(5), *o.TailLines)
		assert.Equal(t, int64(100), *o.LimitBytes)
		assert.Equal(t, int64(60), *o.SinceSeconds)
		assert.True(t, o.SinceTime.Time.Equal(since))
		assert.True(t, o.Follow)
		assert.Nil(t, shadowLogOptions(keptInitName, api.ContainerLogOpts{}).TailLines)
	})
}
