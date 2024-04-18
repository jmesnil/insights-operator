package workloads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash"
	"sync"
	"time"

	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachinerywait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

var (
	labelSelector = "app.kubernetes.io/name=container-scanner"
)

func gatherWorkloadRuntimeInfos(
	ctx context.Context,
	h hash.Hash,
	coreClient corev1client.CoreV1Interface,
	appClient appsv1client.AppsV1Interface,
	restConfig *rest.Config,
) (workloadRuntimes, error) {
	start := time.Now()

	workloadRuntimeInfos := make(workloadRuntimes)

	klog.Infof("Deploying container scanner...\n")
	containerScannerDaemonSet := newContainerScannerDaemonSet()
	if _, err := appClient.DaemonSets(namespace).Create(ctx, containerScannerDaemonSet, metav1.CreateOptions{}); err != nil {
		return workloadRuntimeInfos, err
	}
	err := apimachinerywait.PollUntilContextCancel(ctx, time.Second*5, true, podsReady(coreClient, labelSelector))
	if err != nil {
		return workloadRuntimeInfos, err
	}
	klog.Infof("Container Scanner deployed and ready")

	containerScannerPods := getContainerScannerPods(coreClient, ctx)

	nodeWorkloadCh := make(chan workloadRuntimes)
	var wg sync.WaitGroup
	wg.Add(len(containerScannerPods))

	for nodeName, containerScannerPod := range containerScannerPods {
		go func(nodeName string, containerScannerPod string) {
			defer wg.Done()
			klog.Infof("Gathering workload runtime info for node %s...\n", nodeName)
			nodeWorkloadCh <- getNodeWorkloadRuntimeInfos(h, coreClient, restConfig, containerScannerPod)
		}(nodeName, containerScannerPod)
	}
	go func() {
		wg.Wait()
		close(nodeWorkloadCh)
	}()

	for infos := range nodeWorkloadCh {
		mergeWorkloads(workloadRuntimeInfos, infos)
	}

	err = appClient.DaemonSets(namespace).Delete(ctx, "container-scanner", metav1.DeleteOptions{})
	if err != nil {
		return workloadRuntimeInfos, err
	}
	klog.Infof("Undeployed container scanner\n")

	klog.Infof("Gather workload runtime infos in %s\n",
		time.Since(start).Round(time.Second).String())

	return workloadRuntimeInfos, nil
}

func newContainerScannerDaemonSet() *appsv1.DaemonSet {
	securityContextPrivileged := true
	hostPathSocket := corev1.HostPathSocket
	labels := map[string]string{"app.kubernetes.io/name": "container-scanner"}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "container-scanner", Namespace: namespace},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "container-scanner-sa",
					HostPID:            true,
					ImagePullSecrets: []corev1.LocalObjectReference{{
						Name: "container-scanner-pull-secret",
					}},
					Containers: []corev1.Container{{
						Name:            "container-scanner",
						Image:           "quay.io/jmesnil/container-scanner:rust-latest",
						ImagePullPolicy: corev1.PullAlways,
						Env: []corev1.EnvVar{{
							Name:  "CONTAINER_RUNTIME_ENDPOINT",
							Value: "unix:///crio.sock",
						}},
						SecurityContext: &corev1.SecurityContext{
							Privileged: &securityContextPrivileged,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
								Add:  []corev1.Capability{"CAP_SYS_ADMIN"},
							}},
						VolumeMounts: []corev1.VolumeMount{{
							MountPath: "/crio.sock",
							Name:      "crio-socket",
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "crio-socket",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/run/crio/crio.sock",
								Type: &hostPathSocket,
							},
						}}},
				},
			},
		},
	}
}

// podsReady is a helper function that can be used to check that the selected pods are ready
func podsReady(coreClient corev1client.CoreV1Interface, selector string) apimachinerywait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		opts := metav1.ListOptions{
			LabelSelector: selector,
		}
		pods, err := coreClient.Pods(namespace).List(ctx, opts)
		if err != nil {
			return false, err
		}
		done := true
		for _, pod := range pods.Items {
			podReady := false
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					podReady = true
					break
				}
			}
			if !podReady {
				done = false
			}
		}
		return done, nil
	}
}

func getContainerScannerPods(
	coreClient corev1client.CoreV1Interface,
	ctx context.Context,
) map[string]string {
	containerScannerPods := make(map[string]string)

	pods, err := coreClient.Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return containerScannerPods
	}

	for _, pod := range pods.Items {
		containerScannerPods[pod.Spec.NodeName] = pod.ObjectMeta.Name
	}
	return containerScannerPods
}

// Merge the node workloads in the global map
func mergeWorkloads(global workloadRuntimes,
	node workloadRuntimes,
) {
	for namespace, nodePodWorkloads := range node {
		if _, exists := global[namespace]; !exists {
			// If the namespace doesn't exist in global, simply assign the value from node.
			global[namespace] = nodePodWorkloads
		} else {
			// If the namespace exists, check the pods
			for podName, containerWorkloads := range nodePodWorkloads {
				if _, exists := global[namespace][podName]; !exists {
					// If the namespace/pod doesn't exist in the global map, assign the value from the node.
					global[namespace][podName] = containerWorkloads
				} else {
					// add the workload from the node
					for containerID, runtimeInfo := range containerWorkloads {
						global[namespace][podName][containerID] = runtimeInfo
					}
				}
			}
		}
	}
}

func transformWorkload(h hash.Hash,
	node map[string]map[string]map[string]map[string]any,
) workloadRuntimes {

	result := make(workloadRuntimes)

	for podNamespace, podWorkloads := range node {
		result[podNamespace] = make(map[string]map[string]workloadRuntimeInfoContainer)
		for podName, containerWorkloads := range podWorkloads {
			result[podNamespace][podName] = make(map[string]workloadRuntimeInfoContainer)
			for containerID, info := range containerWorkloads {
				runtimeInfo := workloadRuntimeInfoContainer{}
				osReleaseID, found := info["os-release-id"]
				if found {
					runtimeInfo.Os = workloadHashString(h, osReleaseID.(string))
				}
				osReleaseVersionID, found := info["os-release-version-id"]
				if found {
					runtimeInfo.OsVersion = workloadHashString(h, osReleaseVersionID.(string))
				}
				runtimeKind, found := info["runtime-kind"]
				if found {
					runtimeInfo.Kind = workloadHashString(h, runtimeKind.(string))
				}
				runtimeKindVersion, found := info["runtime-kind-version"]
				if found {
					runtimeInfo.KindVersion = workloadHashString(h, runtimeKindVersion.(string))
				}
				runtimeKindImplementer, found := info["runtime-kind-implementor"]
				if found {
					runtimeInfo.KindImplementer = workloadHashString(h, runtimeKindImplementer.(string))
				}
				runtimeName, found := info["runtime-name"]
				if found {
					runtimeInfo.Name = workloadHashString(h, runtimeName.(string))
				}
				runtimeVersion, found := info["runtime-version"]
				if found {
					runtimeInfo.Version = workloadHashString(h, runtimeVersion.(string))
				}
				result[podNamespace][podName][containerID] = runtimeInfo

			}
		}
	}
	return result
}

// Get all WorkloadRuntimeInfos for a single Node (using the container scanner pod running on this node)
func getNodeWorkloadRuntimeInfos(h hash.Hash,
	coreClient corev1client.CoreV1Interface,
	restConfig *rest.Config,
	containerScannerPod string,
) workloadRuntimes {
	execCommand := []string{"/scan-containers", "--log-level", "trace"}

	req := coreClient.RESTClient().
		Post().
		Namespace(namespace).
		Name(containerScannerPod).
		Resource("pods").
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: execCommand,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		fmt.Printf("error: %s", err)
	}
	var (
		execOut bytes.Buffer
		execErr bytes.Buffer
	)

	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: &execOut,
		Stderr: &execErr,
		Tty:    false,
	})
	if err != nil {
		fmt.Printf("got scanner error: %s\n", err)
		fmt.Printf("command error output: %s\n", execErr.String())
		fmt.Printf("command output: %s\n", execOut.String())
	} else if execErr.Len() > 0 {
		fmt.Errorf("command execution got stderr: %v", execErr.String())
	}

	scannerOutput := execOut.String()

	var nodeOutput map[string]map[string]map[string]map[string]any
	json.Unmarshal([]byte(scannerOutput), &nodeOutput)

	return transformWorkload(h, nodeOutput)
}
