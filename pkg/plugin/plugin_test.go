package plugin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/dancavallaro/kubectl-unmount/pkg/common"
	"github.com/dancavallaro/kubectl-unmount/pkg/logger"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"
)

var (
	storageClassName = "standard" // Default Kind local-path-provisioner StorageClass
	testenv          env.Environment
)

func TestMain(m *testing.M) {
	testenv = env.New()
	kindClusterName := envconf.RandomName("kubectl-unmount-test", 16)

	// Use pre-defined environment funcs to create a kind cluster prior to test run
	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), kindClusterName),
	)

	testenv.Finish(
		envfuncs.DestroyCluster(kindClusterName),
	)

	os.Exit(testenv.Run(m))
}

func TestRunPlugin(t *testing.T) {
	f := features.New("Scale down Deployment").
		Setup(func(ctx context.Context, t *testing.T, config *envconf.Config) context.Context {
			client := config.Client()

			var deployNS, podNS string
			var podSpec corev1.PodSpec

			deployNS, podSpec = createPVCAndPodSpec(ctx, t, client)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: deployNS,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To[int32](1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "test",
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "test",
							},
						},
						Spec: podSpec,
					},
				},
			}
			if err := client.Resources().Create(ctx, deployment); err != nil {
				t.Fatal(err)
			}
			err := wait.For(conditions.New(client.Resources()).ResourceMatch(deployment, func(object k8s.Object) bool {
				d := object.(*appsv1.Deployment)
				return d.Status.AvailableReplicas == 1 && d.Status.ReadyReplicas == 1
			}))
			if err != nil {
				t.Error(err)
			}

			podNS, podSpec = createPVCAndPodSpec(ctx, t, client)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: podNS,
				},
				Spec: podSpec,
			}
			if err := client.Resources().Create(ctx, pod); err != nil {
				t.Fatal(err)
			}
			err = wait.For(conditions.New(client.Resources()).ResourceMatch(pod, func(object k8s.Object) bool {
				p := object.(*corev1.Pod)
				return p.Status.Phase == corev1.PodRunning
			}))
			if err != nil {
				t.Error(err)
			}

			ctx = context.WithValue(ctx, "deployNS", deployNS)
			ctx = context.WithValue(ctx, "podNS", podNS)

			return ctx
		}).
		Assess("Verify expected Pod is running", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value("podNS").(string)
			out, logs, err := runPlugin(func(cfg *ConfigFlags) {
				*cfg.DryRun = true
				*cfg.Namespace = ns
			})
			require.NoError(t, err)
			require.Contains(t, logs, "Found 1 pods to scale down")
			require.Contains(t, logs, "Found 1 controllers to scale down")
			require.ElementsMatch(t, []string{fmt.Sprintf("Pod/%s/test-pod", ns)}, out)
			return ctx
		}).
		Assess("Verify expected Deployment Pod is running", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value("deployNS").(string)
			out, logs, err := runPlugin(func(cfg *ConfigFlags) {
				*cfg.DryRun = true
				*cfg.Namespace = ns
			})
			require.NoError(t, err)
			require.Contains(t, logs, "Found 1 pods to scale down")
			require.Contains(t, logs, "Found 1 controllers to scale down")
			require.ElementsMatch(t, []string{fmt.Sprintf("Deployment/%s/test-deployment", ns)}, out)
			return ctx
		}).
		Assess("Scale down affected controllers", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			out, logs, err := runPlugin(func(cfg *ConfigFlags) {
				*cfg.DryRun = false
			})
			require.NoError(t, err)
			require.Contains(t, logs, "Scale down complete")
			require.ElementsMatch(t, []string{
				fmt.Sprintf("Pod/%s/test-pod", ctx.Value("podNS").(string)),
				fmt.Sprintf("Deployment/%s/test-deployment", ctx.Value("deployNS").(string)),
			}, out)
			return ctx
		}).
		Assess("Verify Pods are no longer running", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			out, logs, err := runPlugin(func(cfg *ConfigFlags) {
				*cfg.DryRun = true
			})
			require.NoError(t, err)
			require.Contains(t, logs, "No pods found, nothing to do")
			require.Empty(t, out)
			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

func createPVCAndPodSpec(ctx context.Context, t *testing.T, client klient.Client) (string, corev1.PodSpec) {
	// Create a random namespace
	namespace := envconf.RandomName("test-ns", 16)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := client.Resources().Create(ctx, ns); err != nil {
		t.Fatal(err)
	}

	// Create a PVC with default StorageClass
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pvc",
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClassName,
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Mi"),
				},
			},
		},
	}
	if err := client.Resources().Create(ctx, pvc); err != nil {
		t.Fatal(err)
	}

	// Create a Pod that uses the PVC
	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "test-container",
				Image: "busybox:latest",
				Command: []string{
					"sh",
					"-c",
					"sleep 3600",
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "test-volume",
						MountPath: "/data",
					},
				},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: "test-volume",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "test-pvc",
					},
				},
			},
		},
	}
	return namespace, podSpec
}

func runPlugin(configurers ...func(*ConfigFlags)) ([]string, string, error) {
	var outBuf, logBuf bytes.Buffer
	pluginCfg := &ConfigFlags{
		ConfigFlags: genericclioptions.ConfigFlags{
			Namespace: common.StringP(""),
		},
		PVCName:      common.StringP(""),
		StorageClass: &storageClassName,
		DryRun:       common.BoolP(false),
		Confirmed:    common.BoolP(true),
		logger:       logger.NewLogger(&logBuf),
		out:          &outBuf,
	}

	for _, configurer := range configurers {
		configurer(pluginCfg)
	}

	err := RunPlugin(pluginCfg)

	return getLines(outBuf.String()), logBuf.String(), err
}

func getLines(s string) []string {
	var lines []string
	for line := range strings.Lines(s) {
		lines = append(lines, strings.TrimSpace(line))
	}
	return lines
}
