package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

const (
	jobTypeBackup  = "backup"
	jobTypeRestore = "restore"
)

// BackupResult is the JSON the backup worker writes to its result ConfigMap.
type BackupResult struct {
	BackupID    string `json:"backupID"`
	TotalPoints int    `json:"totalPoints"`
	SizeBytes   int64  `json:"sizeBytes"`
	Location    string `json:"location"`
}

// RestoreResult is the JSON the restore worker writes to its result ConfigMap.
type RestoreResult struct {
	RestoredPoints int `json:"restoredPoints"`
}

// buildBackupJob renders a Job that exports vector store data to S3.
func buildBackupJob(ctx context.Context, kb *ragv1alpha1.KnowledgeBase, bkp *ragv1alpha1.Backup, backupID string) (*batchv1.Job, string, error) {
	specJSON, err := marshalEffectiveSpec(kb, effectiveChunking(kb))
	if err != nil {
		return nil, "", err
	}

	s3 := bkp.Spec.Destination.S3
	name := truncName(fmt.Sprintf("%s-backup-%s", bkp.Name, backupID))

	env := []corev1.EnvVar{
		{Name: "BACKUP_ID", Value: backupID},
		{Name: "BACKUP_S3_ENDPOINT", Value: s3.Endpoint},
		{Name: "BACKUP_S3_REGION", Value: s3.Region},
		{Name: "BACKUP_S3_BUCKET", Value: s3.Bucket},
		{Name: "BACKUP_S3_PREFIX", Value: s3.Prefix},
	}
	if s3.AccessKeySecretRef.Name != "" {
		env = append(env, secretEnv("BACKUP_S3_ACCESS_KEY", &s3.AccessKeySecretRef))
	}
	if s3.SecretKeySecretRef.Name != "" {
		env = append(env, secretEnv("BACKUP_S3_SECRET_KEY", &s3.SecretKeySecretRef))
	}

	backoff := int32(1)
	ttl := int32(600)
	activeDeadline := int64(3600)

	labels := map[string]string{
		labelManagedBy: "kuberag",
		labelKB:        kb.Name,
		labelJobType:   jobTypeBackup,
	}

	resources, err := resourceRequirements(kb.Spec.Ingestion.Resources)
	if err != nil {
		return nil, "", err
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: bkp.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &activeDeadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					ServiceAccountName:            workerServiceAccountName(kb),
					PriorityClassName:             "kuberag-system",
					TerminationGracePeriodSeconds: ptr.To(int64(120)),
					SecurityContext:               hardenedPodSecurityContext(),
					Volumes:                       []corev1.Volume{scratchVolume(kb.Spec.Ingestion.ModelCacheSizeLimit), specVolume(specConfigMapName(name))},
					NodeSelector:                  kb.Spec.Ingestion.NodeSelector,
					Tolerations:                   kb.Spec.Ingestion.Tolerations,
					Affinity:                      kb.Spec.Ingestion.Affinity,
					Containers: []corev1.Container{
						{
							Name:            "worker",
							Image:           workerImage(kb),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            []string{"backup"},
							Env: append([]corev1.EnvVar{
								{Name: "KB_NAME", Value: kb.Name},
								{Name: "KB_NAMESPACE", Value: kb.Namespace},
								{Name: "KB_SPEC_PATH", Value: "/etc/kuberag/spec.json"},
								{Name: "RESULT_CONFIGMAP", Value: resultConfigMapName(name)},
							}, append(scratchEnv(), append(env, append(traceEnv(ctx), credentialEnv(kb)...)...)...)...),
							Resources:       resources,
							SecurityContext: hardenedContainerSecurityContext(),
							VolumeMounts:    []corev1.VolumeMount{scratchMount(), specMount()},
						},
					},
				},
			},
		},
	}
	return job, specJSON, nil
}

// buildRestoreJob renders a Job that imports vector store data from S3.
func buildRestoreJob(ctx context.Context, kb *ragv1alpha1.KnowledgeBase, rest *ragv1alpha1.Restore, backup *ragv1alpha1.Backup) (*batchv1.Job, string, error) {
	specJSON, err := marshalEffectiveSpec(kb, effectiveChunking(kb))
	if err != nil {
		return nil, "", err
	}

	dst := backup.Spec.Destination.S3
	name := truncName(fmt.Sprintf("%s-restore-%d", rest.Name, time.Now().Unix()))

	env := []corev1.EnvVar{
		{Name: "RESTORE_LOCATION", Value: backup.Status.Location},
		{Name: "RESTORE_S3_ENDPOINT", Value: dst.Endpoint},
		{Name: "RESTORE_S3_REGION", Value: dst.Region},
		{Name: "RESTORE_S3_BUCKET", Value: dst.Bucket},
	}
	if dst.AccessKeySecretRef.Name != "" {
		env = append(env, secretEnv("RESTORE_S3_ACCESS_KEY", &dst.AccessKeySecretRef))
	}
	if dst.SecretKeySecretRef.Name != "" {
		env = append(env, secretEnv("RESTORE_S3_SECRET_KEY", &dst.SecretKeySecretRef))
	}

	backoff := int32(1)
	ttl := int32(600)
	activeDeadline := int64(3600)

	labels := map[string]string{
		labelManagedBy: "kuberag",
		labelKB:        kb.Name,
		labelJobType:   jobTypeRestore,
	}

	resources, err := resourceRequirements(kb.Spec.Ingestion.Resources)
	if err != nil {
		return nil, "", err
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: rest.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &activeDeadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					ServiceAccountName:            workerServiceAccountName(kb),
					PriorityClassName:             "kuberag-system",
					TerminationGracePeriodSeconds: ptr.To(int64(120)),
					SecurityContext:               hardenedPodSecurityContext(),
					Volumes:                       []corev1.Volume{scratchVolume(kb.Spec.Ingestion.ModelCacheSizeLimit), specVolume(specConfigMapName(name))},
					NodeSelector:                  kb.Spec.Ingestion.NodeSelector,
					Tolerations:                   kb.Spec.Ingestion.Tolerations,
					Affinity:                      kb.Spec.Ingestion.Affinity,
					Containers: []corev1.Container{
						{
							Name:            "worker",
							Image:           workerImage(kb),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            []string{"restore"},
							Env: append([]corev1.EnvVar{
								{Name: "KB_NAME", Value: kb.Name},
								{Name: "KB_NAMESPACE", Value: kb.Namespace},
								{Name: "KB_SPEC_PATH", Value: "/etc/kuberag/spec.json"},
								{Name: "RESULT_CONFIGMAP", Value: resultConfigMapName(name)},
							}, append(scratchEnv(), append(env, append(traceEnv(ctx), credentialEnv(kb)...)...)...)...),
							Resources:       resources,
							SecurityContext: hardenedContainerSecurityContext(),
							VolumeMounts:    []corev1.VolumeMount{scratchMount(), specMount()},
						},
					},
				},
			},
		},
	}
	return job, specJSON, nil
}

// readJobResult fetches and parses a worker result ConfigMap using a generic client.
func readJobResult(ctx context.Context, c client.Client, ns, jobName string, out any) error {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: ns, Name: resultConfigMapName(jobName)}
	if err := c.Get(ctx, key, &cm); err != nil {
		return err
	}
	raw, ok := cm.Data["result.json"]
	if !ok {
		return fmt.Errorf("result configmap %s missing result.json", key.Name)
	}
	return json.Unmarshal([]byte(raw), out)
}

// deleteResultCM removes a consumed result ConfigMap (best-effort) using the given client.
func deleteResultCM(ctx context.Context, c client.Client, ns, jobName string) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: resultConfigMapName(jobName)}}
	_ = c.Delete(ctx, cm)
}

// deleteSpecCM removes a worker spec ConfigMap (best-effort) using the given client.
func deleteSpecCM(ctx context.Context, c client.Client, ns, jobName string) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: specConfigMapName(jobName)}}
	_ = c.Delete(ctx, cm)
}
