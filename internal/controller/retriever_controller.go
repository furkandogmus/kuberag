package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

// RetrieverReconciler manages the serving Deployment + Service for a Retriever.
type RetrieverReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=rag.furkan.dev,resources=retrievers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=retrievers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

func (r *RetrieverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rt ragv1alpha1.Retriever
	if err := r.Get(ctx, req.NamespacedName, &rt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve the referenced KnowledgeBase to wire store/model into the server.
	var kb ragv1alpha1.KnowledgeBase
	kbKey := types.NamespacedName{Namespace: rt.Namespace, Name: rt.Spec.KnowledgeBaseRef.Name}
	if err := r.Get(ctx, kbKey, &kb); err != nil {
		if apierrors.IsNotFound(err) {
			rt.Status.Phase = "Pending"
			setRetrieverCond(&rt, ragv1alpha1.ConditionAvailable, metav1.ConditionFalse,
				"KnowledgeBaseNotFound", "referenced KnowledgeBase not found")
			return ctrl.Result{}, r.Status().Update(ctx, &rt)
		}
		return ctrl.Result{}, err
	}

	dep := r.desiredDeployment(&rt, &kb)
	if err := controllerutil.SetControllerReference(&rt, dep, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyDeployment(ctx, dep); err != nil {
		return ctrl.Result{}, err
	}

	svc := r.desiredService(&rt)
	if err := controllerutil.SetControllerReference(&rt, svc, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyService(ctx, svc); err != nil {
		return ctrl.Result{}, err
	}

	// Reflect observed Deployment state into status.
	var live appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name}, &live); err != nil {
		return ctrl.Result{}, err
	}
	rt.Status.ReadyReplicas = live.Status.ReadyReplicas
	rt.Status.ObservedGeneration = rt.Generation
	rt.Status.ServiceEndpoint = fmt.Sprintf("http://%s.%s.svc:8000", svc.Name, svc.Namespace)
	if live.Status.ReadyReplicas >= rt.Spec.Replicas && rt.Spec.Replicas > 0 {
		rt.Status.Phase = "Available"
		setRetrieverCond(&rt, ragv1alpha1.ConditionAvailable, metav1.ConditionTrue, "MinimumReplicasAvailable",
			fmt.Sprintf("%d/%d replicas ready", live.Status.ReadyReplicas, rt.Spec.Replicas))
	} else {
		rt.Status.Phase = "Progressing"
		setRetrieverCond(&rt, ragv1alpha1.ConditionAvailable, metav1.ConditionFalse, "Progressing",
			fmt.Sprintf("%d/%d replicas ready", live.Status.ReadyReplicas, rt.Spec.Replicas))
	}
	return ctrl.Result{}, r.Status().Update(ctx, &rt)
}

func retrieverImage(rt *ragv1alpha1.Retriever) string {
	if rt.Spec.Image != "" {
		return rt.Spec.Image
	}
	return defaultRetrieverImage
}

func boolPtrVal(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func (r *RetrieverReconciler) desiredDeployment(rt *ragv1alpha1.Retriever, kb *ragv1alpha1.KnowledgeBase) *appsv1.Deployment {
	labels := map[string]string{
		labelManagedBy:             "kuberag",
		"rag.furkan.dev/retriever": rt.Name,
	}
	replicas := rt.Spec.Replicas
	collection := kb.Spec.VectorStore.Collection
	if collection == "" {
		collection = kb.Name
	}
	rerankEnabled := rt.Spec.Rerank != nil && boolPtrVal(rt.Spec.Rerank.Enabled, false)
	rerankModel := ""
	if rt.Spec.Rerank != nil {
		rerankModel = rt.Spec.Rerank.Model
	}

	emb := kb.Spec.Embedding
	env := []corev1.EnvVar{
		{Name: "VECTORSTORE_TYPE", Value: string(kb.Spec.VectorStore.Type)},
		{Name: "VECTORSTORE_ENDPOINT", Value: kb.Spec.VectorStore.Endpoint},
		{Name: "VECTORSTORE_COLLECTION", Value: collection},
		{Name: "EMBEDDING_MODEL", Value: emb.Model},
		// Query embedding must use the same provider as ingestion.
		{Name: "EMBEDDING_PROVIDER", Value: emb.Provider},
		{Name: "EMBEDDING_BASE_URL", Value: emb.BaseURL},
		{Name: "EMBEDDING_DIMENSION", Value: fmt.Sprintf("%d", emb.Dimension)},
		{Name: "TOPK", Value: fmt.Sprintf("%d", defaultInt(rt.Spec.TopK, 8))},
		{Name: "SCORE_THRESHOLD", Value: fmt.Sprintf("%d", rt.Spec.ScoreThresholdPercent)},
		{Name: "RERANK_ENABLED", Value: fmt.Sprintf("%t", rerankEnabled)},
		{Name: "RERANK_MODEL", Value: rerankModel},
	}
	if kb.Spec.VectorStore.CredentialsSecretRef != nil {
		env = append(env, secretEnv("VECTORSTORE_CREDENTIAL", kb.Spec.VectorStore.CredentialsSecretRef))
	}
	if emb.APIKeySecretRef != nil {
		env = append(env, secretEnv("EMBEDDING_API_KEY", emb.APIKeySecretRef))
	}

	// Optional LLM answer synthesis (full RAG).
	if g := rt.Spec.Generation; g != nil && boolPtrVal(g.Enabled, true) {
		env = append(env,
			corev1.EnvVar{Name: "GEN_ENABLED", Value: "true"},
			corev1.EnvVar{Name: "GEN_PROVIDER", Value: g.Provider},
			corev1.EnvVar{Name: "GEN_MODEL", Value: g.Model},
			corev1.EnvVar{Name: "GEN_BASE_URL", Value: g.BaseURL},
			corev1.EnvVar{Name: "GEN_MAX_TOKENS", Value: fmt.Sprintf("%d", defaultInt(g.MaxTokens, 512))},
			corev1.EnvVar{Name: "GEN_SYSTEM_PROMPT", Value: g.SystemPrompt},
		)
		if g.APIKeySecretRef != nil {
			env = append(env, secretEnv("GEN_API_KEY", g.APIKeySecretRef))
		}
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rt.Name + "-retriever",
			Namespace: rt.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "retriever",
							Image:           retrieverImage(rt),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports:           []corev1.ContainerPort{{ContainerPort: 8000, Name: "http"}},
							Env:             env,
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt32(8000),
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       10,
							},
						},
					},
				},
			},
		},
	}
}

func (r *RetrieverReconciler) desiredService(rt *ragv1alpha1.Retriever) *corev1.Service {
	labels := map[string]string{
		labelManagedBy:             "kuberag",
		"rag.furkan.dev/retriever": rt.Name,
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rt.Name + "-retriever",
			Namespace: rt.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8000, TargetPort: intstr.FromInt32(8000)},
			},
		},
	}
}

func (r *RetrieverReconciler) applyDeployment(ctx context.Context, dep *appsv1.Deployment) error {
	var existing appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, dep)
	}
	if err != nil {
		return err
	}
	existing.Spec = dep.Spec
	existing.Labels = dep.Labels
	return r.Update(ctx, &existing)
}

func (r *RetrieverReconciler) applyService(ctx context.Context, svc *corev1.Service) error {
	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}
	// Preserve immutable/cluster-assigned fields; only sync selector and ports.
	existing.Spec.Selector = svc.Spec.Selector
	existing.Spec.Ports = svc.Spec.Ports
	return r.Update(ctx, &existing)
}

func setRetrieverCond(rt *ragv1alpha1.Retriever, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&rt.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: rt.Generation,
	})
}

func (r *RetrieverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ragv1alpha1.Retriever{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
