package controller

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

func workerServiceAccountName(kb *ragv1alpha1.KnowledgeBase) string {
	if kb.Spec.Ingestion.ServiceAccountName != "" {
		return kb.Spec.Ingestion.ServiceAccountName
	}
	return truncName(kb.Name + "-worker")
}

func (r *KnowledgeBaseReconciler) validateCustomWorkerServiceAccount(
	ctx context.Context,
	kb *ragv1alpha1.KnowledgeBase,
) error {
	if kb.Spec.Ingestion.ServiceAccountName == "" {
		return nil
	}
	var sa corev1.ServiceAccount
	key := types.NamespacedName{
		Namespace: kb.Namespace,
		Name:      kb.Spec.Ingestion.ServiceAccountName,
	}
	if err := r.Get(ctx, key, &sa); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("ServiceAccount %q not found", key.Name)
		}
		return err
	}
	return nil
}

func (r *KnowledgeBaseReconciler) ensureWorkerIdentity(
	ctx context.Context,
	kb *ragv1alpha1.KnowledgeBase,
	allowedConfigMaps ...string,
) error {
	if kb.Spec.Ingestion.ServiceAccountName != "" {
		return nil
	}

	name := workerServiceAccountName(kb)
	labels := map[string]string{
		labelManagedBy: "kuberag",
		labelKB:        kb.Name,
	}
	sa := &corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: name, Namespace: kb.Namespace, Labels: labels},
		AutomountServiceAccountToken: ptrBool(true),
	}
	if err := controllerutil.SetControllerReference(kb, sa, r.Scheme); err != nil {
		return err
	}
	if err := r.applyWorkerServiceAccount(ctx, sa); err != nil {
		return err
	}

	unique := map[string]struct{}{}
	for _, configMap := range allowedConfigMaps {
		if configMap != "" {
			unique[configMap] = struct{}{}
		}
	}
	resourceNames := make([]string, 0, len(unique))
	for configMap := range unique {
		resourceNames = append(resourceNames, configMap)
	}
	sort.Strings(resourceNames)

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: kb.Namespace, Labels: labels},
	}
	if len(resourceNames) > 0 {
		role.Rules = []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"configmaps"},
			ResourceNames: resourceNames,
			Verbs:         []string{"get", "update", "patch"},
		}}
	}
	if err := controllerutil.SetControllerReference(kb, role, r.Scheme); err != nil {
		return err
	}
	if len(resourceNames) == 0 {
		var existing rbacv1.Role
		key := types.NamespacedName{Namespace: role.Namespace, Name: role.Name}
		if err := r.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
			if err := r.Create(ctx, role); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	} else {
		if err := r.applyWorkerRole(ctx, role); err != nil {
			return err
		}
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: kb.Namespace, Labels: labels},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     name,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      name,
			Namespace: kb.Namespace,
		}},
	}
	if err := controllerutil.SetControllerReference(kb, binding, r.Scheme); err != nil {
		return err
	}
	return r.applyWorkerRoleBinding(ctx, binding)
}

func ptrBool(v bool) *bool { return &v }

func (r *KnowledgeBaseReconciler) applyWorkerServiceAccount(ctx context.Context, desired *corev1.ServiceAccount) error {
	var existing corev1.ServiceAccount
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	if err := r.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	existing.AutomountServiceAccountToken = desired.AutomountServiceAccountToken
	return r.Update(ctx, &existing)
}

func (r *KnowledgeBaseReconciler) applyWorkerRole(ctx context.Context, desired *rbacv1.Role) error {
	var existing rbacv1.Role
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	if err := r.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	existing.Rules = desired.Rules
	return r.Update(ctx, &existing)
}

func (r *KnowledgeBaseReconciler) applyWorkerRoleBinding(ctx context.Context, desired *rbacv1.RoleBinding) error {
	var existing rbacv1.RoleBinding
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	if err := r.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	existing.RoleRef = desired.RoleRef
	existing.Subjects = desired.Subjects
	return r.Update(ctx, &existing)
}

func (r *KnowledgeBaseReconciler) prepareWorkerJob(
	ctx context.Context,
	kb *ragv1alpha1.KnowledgeBase,
	jobName string,
	additionalConfigMaps ...string,
) error {
	resultName := resultConfigMapName(jobName)
	result := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resultName,
			Namespace: kb.Namespace,
			Labels: map[string]string{
				labelManagedBy: "kuberag",
				labelKB:        kb.Name,
			},
		},
		Data: map[string]string{},
	}
	if err := controllerutil.SetControllerReference(kb, result, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, result); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	checkpointName := checkpointConfigMapName(jobName)
	checkpoint := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      checkpointName,
			Namespace: kb.Namespace,
			Labels: map[string]string{
				labelManagedBy: "kuberag",
				labelKB:        kb.Name,
			},
		},
		Data: map[string]string{},
	}
	if err := controllerutil.SetControllerReference(kb, checkpoint, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, checkpoint); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	allowed := append([]string{resultName, checkpointName}, additionalConfigMaps...)
	return r.ensureWorkerIdentity(ctx, kb, allowed...)
}

func (r *KnowledgeBaseReconciler) clearWorkerConfigMapAccess(ctx context.Context, kb *ragv1alpha1.KnowledgeBase) {
	if kb.Spec.Ingestion.ServiceAccountName != "" {
		return
	}
	name := workerServiceAccountName(kb)
	var role rbacv1.Role
	key := types.NamespacedName{Namespace: kb.Namespace, Name: name}
	if err := r.Get(ctx, key, &role); err != nil {
		return
	}
	if len(role.Rules) == 0 {
		return
	}
	role.Rules = nil
	_ = r.Update(ctx, &role)
}
