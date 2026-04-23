//go:build bench

package bench

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	km "github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta2"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	t4MinIONamespace       = "bench-t4-system"
	t4MinIOServiceName     = "bench-t4-minio"
	t4MinIOCredsSecretName = "bench-t4-minio-creds"
	t4EmbeddedK0sImage     = "makhov/k0s"
	t4EmbeddedK0sVersion   = "v1.35.1-k0s.1-t4-4-amd64"
	t4DefaultBucket        = "bench-t4"
	t4DefaultAccessKey     = "benchminio"
	t4DefaultSecretKey     = "benchminiosecret"
	t4BusyboxImage         = "busybox:1.36"
)

func perfStorageConfigs(k0sVersion string) []storageEntry {
	configs := append([]storageEntry{}, storageConfigs(k0sVersion)...)

	if t4URL := strings.TrimSpace(os.Getenv("BENCH_T4_URL")); t4URL != "" {
		configs = append(configs, storageEntry{
			StorageName: "kine-t4",
			Enabled:     true,
			StorageType: km.StorageTypeKine,
			StorageKine: km.KineSpec{DataSourceURL: t4URL},
		})
	}

	if os.Getenv("BENCH_T4_EMBEDDED_ENABLED") != "0" {
		configs = append(configs, storageEntry{
			StorageName: "kine-t4-embedded",
			Enabled:     true,
			StorageType: km.StorageTypeKine,
		})
	}

	return configs
}

func configurePerfScenario(ctx context.Context, sc storageEntry, clusterName, namespace string, nodeAddrs []string, replicas int32, k0sVersion string) (ScenarioConfig, error) {
	cfg := ScenarioConfig{
		StorageName:     sc.StorageName,
		StorageType:     sc.StorageType,
		StorageKine:     sc.StorageKine,
		StorageEtcd:     sc.StorageEtcd,
		StorageNATS:     sc.StorageNATS,
		ServiceType:     corev1.ServiceTypeNodePort,
		ExternalAddress: nodeAddrs[0],
		APISANs:         nodeAddrs,
		HCPReplicas:     replicas,
		K0sVersion:      k0sVersion,
		Namespace:       namespace,
	}

	if sc.StorageName != "kine-t4-embedded" {
		return cfg, nil
	}

	bucket := t4BucketName(clusterName)
	endpoint, accessKey, secretKey, err := ensureT4MinIO(ctx, bucket)
	if err != nil {
		return cfg, err
	}

	headlessServiceName := clusterName + "-t4-headless"
	if err := ensureT4HeadlessService(ctx, namespace, clusterName, headlessServiceName); err != nil {
		return cfg, err
	}

	cfg.Image = envOrDefault("BENCH_T4_EMBEDDED_IMAGE", t4EmbeddedK0sImage)
	cfg.K0sVersion = envOrDefault("BENCH_T4_EMBEDDED_VERSION", t4EmbeddedK0sVersion)
	cfg.StorageKine = km.KineSpec{
		DataSourceURL: fmt.Sprintf(
			"t4://%s/%s?service-name=%s.%s.svc.cluster.local&data-dir=/tmp/t4&s3-endpoint=%s&segment-max-age=60s",
			bucket,
			clusterName,
			headlessServiceName,
			namespace,
			endpoint,
		),
	}
	cfg.Patches = []km.ComponentPatch{
		{
			Target: km.PatchTarget{
				Kind:      "StatefulSet",
				Component: "control-plane",
			},
			Patch: km.PatchSpec{
				Type: km.StrategicMergePatchType,
				Content: fmt.Sprintf(`spec:
  podManagementPolicy: Parallel
  serviceName: %s
  template:
    spec:
      initContainers:
        - name: wait-dns
          image: %s
          command:
            - sh
            - -c
            - |
              until nslookup $(hostname).%s.%s.svc.cluster.local; do
                echo "waiting for own DNS..."; sleep 2
              done
      containers:
        - name: controller
          env:
            - name: T4_S3_ACCESS_KEY_ID
              value: %q
            - name: T4_S3_SECRET_ACCESS_KEY
              value: %q
`, headlessServiceName, t4BusyboxImage, headlessServiceName, namespace, accessKey, secretKey),
			},
		},
	}
	return cfg, nil
}

func ensureT4HeadlessService(ctx context.Context, namespace, clusterName, serviceName string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector: map[string]string{
				"app":       "k0smotron",
				"cluster":   clusterName,
				"component": "cluster",
			},
			Ports: []corev1.ServicePort{
				{Name: "t4-http", Port: 3379, TargetPort: intstr.FromInt(3379)},
				{Name: "t4-cluster", Port: 3380, TargetPort: intstr.FromInt(3380)},
			},
		},
	}

	current, err := globalKC.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = globalKC.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get t4 headless service: %w", err)
	}

	svc.ResourceVersion = current.ResourceVersion
	_, err = globalKC.CoreV1().Services(namespace).Update(ctx, svc, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update t4 headless service: %w", err)
	}
	return nil
}

func ensureT4MinIO(ctx context.Context, bucket string) (endpoint, accessKey, secretKey string, err error) {
	endpoint = strings.TrimSpace(os.Getenv("BENCH_T4_S3_ENDPOINT"))
	accessKey = envOrDefault("BENCH_T4_S3_ACCESS_KEY_ID", t4DefaultAccessKey)
	secretKey = envOrDefault("BENCH_T4_S3_SECRET_ACCESS_KEY", t4DefaultSecretKey)
	if bucket == "" {
		bucket = envOrDefault("BENCH_T4_S3_BUCKET", t4DefaultBucket)
	}
	if endpoint != "" {
		return endpoint, accessKey, secretKey, nil
	}

	if err := ensureNamespace(ctx, globalKC, t4MinIONamespace); err != nil {
		return "", "", "", fmt.Errorf("ensure t4 namespace: %w", err)
	}
	if err := ensureT4MinIOSecret(ctx, accessKey, secretKey); err != nil {
		return "", "", "", err
	}
	if err := ensureT4MinIOService(ctx); err != nil {
		return "", "", "", err
	}
	if err := ensureT4MinIODeployment(ctx); err != nil {
		return "", "", "", err
	}
	if err := waitDeploymentReady(ctx, t4MinIONamespace, t4MinIOServiceName, 5*time.Minute); err != nil {
		return "", "", "", err
	}
	if err := ensureT4Bucket(ctx, accessKey, secretKey, bucket); err != nil {
		return "", "", "", err
	}

	return fmt.Sprintf("http://%s.%s.svc.cluster.local:9000", t4MinIOServiceName, t4MinIONamespace), accessKey, secretKey, nil
}

func ensureT4MinIOSecret(ctx context.Context, accessKey, secretKey string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t4MinIOCredsSecretName,
			Namespace: t4MinIONamespace,
		},
		StringData: map[string]string{
			"root-user":     accessKey,
			"root-password": secretKey,
		},
	}
	current, err := globalKC.CoreV1().Secrets(t4MinIONamespace).Get(ctx, t4MinIOCredsSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = globalKC.CoreV1().Secrets(t4MinIONamespace).Create(ctx, secret, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get t4 minio secret: %w", err)
	}
	secret.ResourceVersion = current.ResourceVersion
	_, err = globalKC.CoreV1().Secrets(t4MinIONamespace).Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update t4 minio secret: %w", err)
	}
	return nil
}

func ensureT4MinIOService(ctx context.Context) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t4MinIOServiceName,
			Namespace: t4MinIONamespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": t4MinIOServiceName},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       9000,
				TargetPort: intstr.FromInt(9000),
			}},
		},
	}
	current, err := globalKC.CoreV1().Services(t4MinIONamespace).Get(ctx, t4MinIOServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = globalKC.CoreV1().Services(t4MinIONamespace).Create(ctx, svc, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get t4 minio service: %w", err)
	}
	svc.ResourceVersion = current.ResourceVersion
	svc.Spec.ClusterIP = current.Spec.ClusterIP
	svc.Spec.ClusterIPs = current.Spec.ClusterIPs
	svc.Spec.IPFamilies = current.Spec.IPFamilies
	svc.Spec.IPFamilyPolicy = current.Spec.IPFamilyPolicy
	_, err = globalKC.CoreV1().Services(t4MinIONamespace).Update(ctx, svc, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update t4 minio service: %w", err)
	}
	return nil
}

func ensureT4MinIODeployment(ctx context.Context) error {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t4MinIOServiceName,
			Namespace: t4MinIONamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": t4MinIOServiceName},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": t4MinIOServiceName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "minio",
						Image: envOrDefault("BENCH_T4_MINIO_IMAGE", "minio/minio:latest"),
						Args:  []string{"server", "/data", "--address", ":9000"},
						Env: []corev1.EnvVar{
							{
								Name: "MINIO_ROOT_USER",
								ValueFrom: &corev1.EnvVarSource{
									SecretKeyRef: &corev1.SecretKeySelector{
										LocalObjectReference: corev1.LocalObjectReference{Name: t4MinIOCredsSecretName},
										Key:                  "root-user",
									},
								},
							},
							{
								Name: "MINIO_ROOT_PASSWORD",
								ValueFrom: &corev1.EnvVarSource{
									SecretKeyRef: &corev1.SecretKeySelector{
										LocalObjectReference: corev1.LocalObjectReference{Name: t4MinIOCredsSecretName},
										Key:                  "root-password",
									},
								},
							},
						},
						Ports: []corev1.ContainerPort{{ContainerPort: 9000}},
					}},
				},
			},
		},
	}

	current, err := globalKC.AppsV1().Deployments(t4MinIONamespace).Get(ctx, t4MinIOServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = globalKC.AppsV1().Deployments(t4MinIONamespace).Create(ctx, dep, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get t4 minio deployment: %w", err)
	}
	dep.ResourceVersion = current.ResourceVersion
	_, err = globalKC.AppsV1().Deployments(t4MinIONamespace).Update(ctx, dep, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update t4 minio deployment: %w", err)
	}
	return nil
}

func ensureT4Bucket(ctx context.Context, accessKey, secretKey, bucket string) error {
	jobName := t4BucketJobName(bucket)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: t4MinIONamespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"job": jobName},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:  "mc",
						Image: envOrDefault("BENCH_T4_MC_IMAGE", "minio/mc:latest"),
						Command: []string{"sh", "-c", fmt.Sprintf(
							`mc alias set local http://%s.%s.svc.cluster.local:9000 %q %q && mc mb --ignore-existing local/%s`,
							t4MinIOServiceName,
							t4MinIONamespace,
							accessKey,
							secretKey,
							bucket,
						)},
					}},
				},
			},
		},
	}

	current, err := globalKC.BatchV1().Jobs(t4MinIONamespace).Get(ctx, jobName, metav1.GetOptions{})
	if err == nil {
		if current.Status.Succeeded > 0 {
			return nil
		}
		if delErr := globalKC.BatchV1().Jobs(t4MinIONamespace).Delete(ctx, jobName, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("delete stale t4 bucket job: %w", delErr)
		}
		if err := waitJobDeleted(ctx, jobName); err != nil {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get t4 bucket job: %w", err)
	}

	if _, err := globalKC.BatchV1().Jobs(t4MinIONamespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create t4 bucket job: %w", err)
	}
	return waitJobComplete(ctx, jobName, 5*time.Minute)
}

func waitDeploymentReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		dep, err := globalKC.AppsV1().Deployments(namespace).Get(waitCtx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
		}
		if dep.Status.ReadyReplicas >= 1 {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func waitJobComplete(ctx context.Context, name string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		job, err := globalKC.BatchV1().Jobs(t4MinIONamespace).Get(waitCtx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get job %s/%s: %w", t4MinIONamespace, name, err)
		}
		if job.Status.Succeeded > 0 {
			return nil
		}
		if job.Status.Failed > 0 && job.Spec.BackoffLimit != nil && job.Status.Failed >= *job.Spec.BackoffLimit {
			return fmt.Errorf("job %s/%s failed", t4MinIONamespace, name)
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func waitJobDeleted(ctx context.Context, name string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		_, err := globalKC.BatchV1().Jobs(t4MinIONamespace).Get(waitCtx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get job while waiting for deletion: %w", err)
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func t4BucketJobName(bucket string) string {
	name := "bench-t4-minio-bucket-" + bucket
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

func t4BucketName(clusterName string) string {
	base := strings.ToLower(clusterName)
	base = strings.ReplaceAll(base, "_", "-")
	if len(base) > 50 {
		base = base[:50]
	}
	return "bench-t4-" + strings.Trim(base, "-")
}

func int32Ptr(v int32) *int32 {
	return &v
}
