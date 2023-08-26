package kaniko

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
)

const (
	defaultNamespace       = "default"
	kanikoImage            = "gcr.io/kaniko-project/executor:v1.5.1"
	inClusterNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

type runOptions struct {
	ID          string
	GitRevision string
	GitUsername string
	GitPassword string

	Context          string
	Dockerfile       string
	Destination      string
	BuildArg         map[string]string
	RegistryUsername string
	RegistryPassword string
	Cache            bool
	NoPush           bool
	Reproducible     bool
	PushRetry        int64
	Verbosity        string
}

type DockerConfigJSON struct {
	Auths map[string]authn.AuthConfig
}

func kanikoBuild(ctx context.Context, restConfig *rest.Config, opts *runOptions) error {
	coreV1Client, err := v1.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	namespace := defaultNamespace
	if _, err = os.Stat(inClusterNamespaceFile); err == nil {
		namespaceBytes, err := os.ReadFile(inClusterNamespaceFile)
		if err == nil {
			namespace = string(namespaceBytes)
		}
	}

	ref, err := name.ParseReference(opts.Destination)
	if err != nil {
		return err
	}
	registry := fmt.Sprintf("https://%s/v1/", ref.Context().RegistryStr())
	secret, err := getDockerConfigSecret(namespace, opts.ID, registry, opts.RegistryUsername, opts.RegistryPassword)
	if err != nil {
		return err
	}

	if _, err := coreV1Client.Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return err
	}

	pod := getKanikoPod(namespace, opts)
	if _, err := coreV1Client.Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return err
	}

	defer func() {
		// Clean up.
		if err = coreV1Client.Pods(namespace).Delete(ctx, opts.ID, metav1.DeleteOptions{}); err != nil {
			fmt.Println("failed to clean up kaniko job", map[string]any{"error": err})
			tflog.Warn(ctx, "failed to clean up kaniko job", map[string]any{"error": err})
		}

		if err = coreV1Client.Secrets(namespace).Delete(ctx, opts.ID, metav1.DeleteOptions{}); err != nil {
			fmt.Println("failed to clean up kaniko secret", map[string]any{"error": err})
			tflog.Warn(ctx, "failed to clean up kaniko secret", map[string]any{"error": err})
		}
	}()

OuterLoop:
	for {
		watch, err := coreV1Client.Pods(namespace).Watch(context.TODO(), metav1.ListOptions{
			FieldSelector:  "metadata.name=" + opts.ID,
			TimeoutSeconds: pointer.Int64Ptr(600), // Set the timeout to 1 hour
		})
		if err != nil {
			panic(err)
		}
		ch := watch.ResultChan()
		for event := range ch {
			pod, ok := event.Object.(*apiv1.Pod)
			if !ok {
				tflog.Warn(ctx, "unexpected k8s resource event", map[string]any{"event": event})
				continue
			}
			if pod.Name != opts.ID {
				continue
			}

			if pod.Status.Phase == apiv1.PodSucceeded {
				// seccess
				break OuterLoop
			}

			if pod.Status.Phase == apiv1.PodFailed {
				logs, err := getJobPodsLogs(ctx, namespace, opts.ID, restConfig)
				if err != nil {
					return fmt.Errorf("kaniko job failed, but cannot get pod logs: %w", err)
				}
				return fmt.Errorf("build logs: %s", logs)
			}
		}
	}

	return nil
}

// getJobPodsLogs returns the logs of all pods of a job.
func getJobPodsLogs(ctx context.Context, namespace, podName string, restConfig *rest.Config) (string, error) {
	clientSet, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", err
	}
	pods, err := clientSet.CoreV1().Pods(namespace).
		List(ctx, metav1.ListOptions{
			FieldSelector: "metadata.name=" + podName,
		})
	if err != nil {
		return "", err
	}

	var logs string
	for _, pod := range pods.Items {
		var podLogs []byte
		podLogs, err = clientSet.CoreV1().Pods(namespace).GetLogs(pod.Name, &apiv1.PodLogOptions{}).DoRaw(ctx)
		if err != nil {
			return "", err
		}
		logs += string(podLogs)
	}

	return logs, nil
}

func getDockerConfigSecret(namespace, name, registry, username, password string) (*apiv1.Secret, error) {
	cfg := DockerConfigJSON{
		Auths: map[string]authn.AuthConfig{
			registry: {
				Username: username,
				Password: password,
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}

	return &apiv1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Data: map[string][]byte{
			"config.json": data,
		},
	}, nil
}

func getKanikoPod(namespace string, opts *runOptions) *apiv1.Pod {
	args := []string{
		fmt.Sprintf("--dockerfile=%s", opts.Dockerfile),
		fmt.Sprintf("--context=%s", opts.Context),
		fmt.Sprintf("--destination=%s", opts.Destination),
		fmt.Sprintf("--push-retry=%d", opts.PushRetry),
		fmt.Sprintf("--verbosity=%s", opts.Verbosity),
	}

	var volumeMounts []apiv1.VolumeMount
	var volumes []apiv1.Volume
	if opts.RegistryUsername != "" && opts.RegistryPassword != "" {
		volumeMounts = append(volumeMounts, apiv1.VolumeMount{
			Name:      "docker-config",
			MountPath: "/kaniko/.docker/",
		})
		volumes = append(volumes, apiv1.Volume{
			Name: "docker-config",
			VolumeSource: apiv1.VolumeSource{
				Secret: &apiv1.SecretVolumeSource{
					SecretName: opts.ID,
				},
			},
		})
	}

	return &apiv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      opts.ID,
		},
		Spec: apiv1.PodSpec{
			Containers: []apiv1.Container{
				{
					Name:         "build",
					Image:        kanikoImage,
					VolumeMounts: volumeMounts,
					Args:         args,
				},
			},
			Volumes:       volumes,
			RestartPolicy: apiv1.RestartPolicyNever,
		},
	}

}
