package controller

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

func baseBackup() *ragv1alpha1.Backup {
	return &ragv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "bkp", Namespace: "default"},
		Spec: ragv1alpha1.BackupSpec{
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "kb"},
			Destination: ragv1alpha1.BackupDestination{
				S3: &ragv1alpha1.S3BackupTarget{
					Endpoint:           "https://s3.amazonaws.com",
					Region:             "us-east-1",
					Bucket:             "my-bucket",
					AccessKeySecretRef: ragv1alpha1.SecretKeyRef{Name: "s3-creds", Key: "accessKey"},
					SecretKeySecretRef: ragv1alpha1.SecretKeyRef{Name: "s3-creds", Key: "secretKey"},
				},
			},
		},
	}
}

func baseRestore() *ragv1alpha1.Restore {
	return &ragv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "rst", Namespace: "default"},
		Spec: ragv1alpha1.RestoreSpec{
			BackupRef:        ragv1alpha1.LocalObjectRef{Name: "bkp"},
			KnowledgeBaseRef: ragv1alpha1.LocalObjectRef{Name: "kb"},
		},
	}
}

func TestBackupCRDValidation(t *testing.T) {
	bkp := baseBackup()

	if bkp.Name != "bkp" {
		t.Fatalf("expected Name bkp, got %q", bkp.Name)
	}
	if bkp.Namespace != "default" {
		t.Fatalf("expected Namespace default, got %q", bkp.Namespace)
	}
	if bkp.Spec.KnowledgeBaseRef.Name != "kb" {
		t.Fatalf("expected KnowledgeBaseRef kb, got %q", bkp.Spec.KnowledgeBaseRef.Name)
	}
	if bkp.Spec.Destination.S3.Endpoint != "https://s3.amazonaws.com" {
		t.Fatalf("unexpected S3 endpoint: %q", bkp.Spec.Destination.S3.Endpoint)
	}
	if bkp.Spec.Destination.S3.Region != "us-east-1" {
		t.Fatalf("unexpected S3 region: %q", bkp.Spec.Destination.S3.Region)
	}
	if bkp.Spec.Destination.S3.Bucket != "my-bucket" {
		t.Fatalf("unexpected S3 bucket: %q", bkp.Spec.Destination.S3.Bucket)
	}
	if bkp.Spec.Destination.S3.AccessKeySecretRef.Name != "s3-creds" {
		t.Fatalf("unexpected access key secret ref name: %q", bkp.Spec.Destination.S3.AccessKeySecretRef.Name)
	}
	if bkp.Spec.Destination.S3.SecretKeySecretRef.Key != "secretKey" {
		t.Fatalf("unexpected secret key ref key: %q", bkp.Spec.Destination.S3.SecretKeySecretRef.Key)
	}

	phases := []ragv1alpha1.BackupPhase{
		ragv1alpha1.BackupPhasePending,
		ragv1alpha1.BackupPhaseRunning,
		ragv1alpha1.BackupPhaseCompleted,
		ragv1alpha1.BackupPhaseFailed,
	}
	for _, p := range phases {
		if p == "" {
			t.Fatal("BackupPhase should not be empty")
		}
	}

	bkp2 := baseBackup()
	bkp2.Name = "bkp2"
	bkp2.Status.Phase = ragv1alpha1.BackupPhaseCompleted
	list := ragv1alpha1.BackupList{Items: []ragv1alpha1.Backup{*bkp, *bkp2}}
	if len(list.Items) != 2 {
		t.Fatalf("BackupList should hold 2 items, got %d", len(list.Items))
	}
	if list.Items[0].Name != "bkp" || list.Items[1].Name != "bkp2" {
		t.Fatal("BackupList items have wrong names")
	}
}

func TestRestoreCRDValidation(t *testing.T) {
	rst := baseRestore()

	if rst.Name != "rst" {
		t.Fatalf("expected Name rst, got %q", rst.Name)
	}
	if rst.Spec.BackupRef.Name != "bkp" {
		t.Fatalf("expected BackupRef bkp, got %q", rst.Spec.BackupRef.Name)
	}
	if rst.Spec.KnowledgeBaseRef.Name != "kb" {
		t.Fatalf("expected KnowledgeBaseRef kb, got %q", rst.Spec.KnowledgeBaseRef.Name)
	}
	if rst.Spec.Suspend {
		t.Fatal("expected Suspend to be false by default")
	}

	phases := []ragv1alpha1.RestorePhase{
		ragv1alpha1.RestorePhasePending,
		ragv1alpha1.RestorePhaseRunning,
		ragv1alpha1.RestorePhaseCompleted,
		ragv1alpha1.RestorePhaseFailed,
	}
	for _, p := range phases {
		if p == "" {
			t.Fatal("RestorePhase should not be empty")
		}
	}

	// Verify RestoreList can hold multiple restores.
	rst2 := baseRestore()
	rst2.Name = "rst2"
	list := ragv1alpha1.RestoreList{Items: []ragv1alpha1.Restore{*rst, *rst2}}
	if len(list.Items) != 2 {
		t.Fatalf("RestoreList should hold 2 items, got %d", len(list.Items))
	}
}

func TestBackupJobBuilder(t *testing.T) {
	kb := baseKB()
	bkp := baseBackup()

	job, _, err := buildBackupJob(context.Background(), kb, bkp, "1718200000")
	if err != nil {
		t.Fatalf("buildBackupJob returned error: %v", err)
	}

	if job.Labels[labelManagedBy] != "kuberag" {
		t.Errorf("expected label %s=kuberag, got %q", labelManagedBy, job.Labels[labelManagedBy])
	}
	if job.Labels[labelKB] != "kb" {
		t.Errorf("expected label %s=kb, got %q", labelKB, job.Labels[labelKB])
	}
	if job.Labels[labelJobType] != jobTypeBackup {
		t.Errorf("expected label %s=backup, got %q", labelJobType, job.Labels[labelJobType])
	}

	container := job.Spec.Template.Spec.Containers[0]
	if len(container.Args) != 1 || container.Args[0] != "backup" {
		t.Errorf("expected args [\"backup\"], got %v", container.Args)
	}

	envMap := make(map[string]string)
	for _, env := range container.Env {
		if env.ValueFrom == nil {
			envMap[env.Name] = env.Value
		}
	}

	if envMap["BACKUP_S3_ENDPOINT"] != "https://s3.amazonaws.com" {
		t.Errorf("unexpected BACKUP_S3_ENDPOINT: %q", envMap["BACKUP_S3_ENDPOINT"])
	}
	if envMap["BACKUP_S3_REGION"] != "us-east-1" {
		t.Errorf("unexpected BACKUP_S3_REGION: %q", envMap["BACKUP_S3_REGION"])
	}
	if envMap["BACKUP_S3_BUCKET"] != "my-bucket" {
		t.Errorf("unexpected BACKUP_S3_BUCKET: %q", envMap["BACKUP_S3_BUCKET"])
	}
	if envMap["BACKUP_ID"] != "1718200000" {
		t.Errorf("unexpected BACKUP_ID: %q", envMap["BACKUP_ID"])
	}

	hasAccessKey, hasSecretKey := false, false
	for _, env := range container.Env {
		if env.Name == "BACKUP_S3_ACCESS_KEY" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			if env.ValueFrom.SecretKeyRef.Name == "s3-creds" && env.ValueFrom.SecretKeyRef.Key == "accessKey" {
				hasAccessKey = true
			}
		}
		if env.Name == "BACKUP_S3_SECRET_KEY" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			if env.ValueFrom.SecretKeyRef.Name == "s3-creds" && env.ValueFrom.SecretKeyRef.Key == "secretKey" {
				hasSecretKey = true
			}
		}
	}
	if !hasAccessKey {
		t.Error("expected BACKUP_S3_ACCESS_KEY env var from secret ref")
	}
	if !hasSecretKey {
		t.Error("expected BACKUP_S3_SECRET_KEY env var from secret ref")
	}

	if job.Namespace != "default" {
		t.Errorf("expected Job namespace=default, got %q", job.Namespace)
	}
}

func TestRestoreJobBuilder(t *testing.T) {
	kb := baseKB()
	rst := baseRestore()

	completedBackup := baseBackup()
	completedBackup.Status.Phase = ragv1alpha1.BackupPhaseCompleted
	completedBackup.Status.Location = "s3://my-bucket/kuberag-backups/bkp-1718200000.tar.gz"

	job, _, err := buildRestoreJob(context.Background(), kb, rst, completedBackup)
	if err != nil {
		t.Fatalf("buildRestoreJob returned error: %v", err)
	}

	if job.Labels[labelManagedBy] != "kuberag" {
		t.Errorf("expected label %s=kuberag, got %q", labelManagedBy, job.Labels[labelManagedBy])
	}
	if job.Labels[labelKB] != "kb" {
		t.Errorf("expected label %s=kb, got %q", labelKB, job.Labels[labelKB])
	}
	if job.Labels[labelJobType] != jobTypeRestore {
		t.Errorf("expected label %s=restore, got %q", labelJobType, job.Labels[labelJobType])
	}

	container := job.Spec.Template.Spec.Containers[0]
	if len(container.Args) != 1 || container.Args[0] != "restore" {
		t.Errorf("expected args [\"restore\"], got %v", container.Args)
	}

	envMap := make(map[string]string)
	for _, env := range container.Env {
		if env.ValueFrom == nil {
			envMap[env.Name] = env.Value
		}
	}

	if envMap["RESTORE_LOCATION"] != completedBackup.Status.Location {
		t.Errorf("unexpected RESTORE_LOCATION: %q", envMap["RESTORE_LOCATION"])
	}
	if envMap["RESTORE_ROUND"] == "" {
		t.Error("expected RESTORE_ROUND for versioned atomic restore")
	}
	if envMap["RESTORE_S3_ENDPOINT"] != "https://s3.amazonaws.com" {
		t.Errorf("unexpected RESTORE_S3_ENDPOINT: %q", envMap["RESTORE_S3_ENDPOINT"])
	}
	if envMap["RESTORE_S3_REGION"] != "us-east-1" {
		t.Errorf("unexpected RESTORE_S3_REGION: %q", envMap["RESTORE_S3_REGION"])
	}
	if envMap["RESTORE_S3_BUCKET"] != "my-bucket" {
		t.Errorf("unexpected RESTORE_S3_BUCKET: %q", envMap["RESTORE_S3_BUCKET"])
	}

	hasAccessKey, hasSecretKey := false, false
	for _, env := range container.Env {
		if env.Name == "RESTORE_S3_ACCESS_KEY" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			if env.ValueFrom.SecretKeyRef.Name == "s3-creds" && env.ValueFrom.SecretKeyRef.Key == "accessKey" {
				hasAccessKey = true
			}
		}
		if env.Name == "RESTORE_S3_SECRET_KEY" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			if env.ValueFrom.SecretKeyRef.Name == "s3-creds" && env.ValueFrom.SecretKeyRef.Key == "secretKey" {
				hasSecretKey = true
			}
		}
	}
	if !hasAccessKey {
		t.Error("expected RESTORE_S3_ACCESS_KEY env var from secret ref")
	}
	if !hasSecretKey {
		t.Error("expected RESTORE_S3_SECRET_KEY env var from secret ref")
	}

	if job.Namespace != "default" {
		t.Errorf("expected Job namespace=default, got %q", job.Namespace)
	}
}

func TestBackupResultMarshal(t *testing.T) {
	result := BackupResult{
		BackupID:    "1718200000",
		TotalPoints: 15000,
		SizeBytes:   1048576,
		Location:    "s3://my-bucket/kuberag-backups/bkp-1718200000.tar.gz",
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal BackupResult: %v", err)
	}

	var decoded BackupResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal BackupResult: %v", err)
	}

	if decoded != result {
		t.Fatalf("round-trip mismatch: %+v != %+v", decoded, result)
	}

	// RestoreResult round-trip.
	rr := RestoreResult{RestoredPoints: 15000}
	data2, err := json.Marshal(rr)
	if err != nil {
		t.Fatalf("failed to marshal RestoreResult: %v", err)
	}

	var decoded2 RestoreResult
	if err := json.Unmarshal(data2, &decoded2); err != nil {
		t.Fatalf("failed to unmarshal RestoreResult: %v", err)
	}

	if decoded2 != rr {
		t.Fatalf("round-trip mismatch: %+v != %+v", decoded2, rr)
	}
}
