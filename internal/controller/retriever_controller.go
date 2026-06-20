package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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

	secretHash := r.computeSecretsHash(ctx, &rt, &kb)
	dep := r.desiredDeployment(&rt, &kb, secretHash)
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

	// Check VectorIndex health before reporting Available.
	var vi ragv1alpha1.VectorIndex
	viKey := types.NamespacedName{Namespace: kb.Namespace, Name: truncName(kb.Name + "-index")}
	viHealthy := true
	viReason := ""
	if err := r.Get(ctx, viKey, &vi); err == nil {
		if vi.Status.Health == ragv1alpha1.IndexMissing || vi.Status.Health == ragv1alpha1.IndexDegraded {
			viHealthy = false
			viReason = fmt.Sprintf("vectorindex %s has health %s", vi.Name, vi.Status.Health)
		}
	}

	if live.Status.ReadyReplicas >= rt.Spec.Replicas && rt.Spec.Replicas > 0 {
		if viHealthy {
			rt.Status.Phase = "Available"
			setRetrieverCond(&rt, ragv1alpha1.ConditionAvailable, metav1.ConditionTrue, "MinimumReplicasAvailable",
				fmt.Sprintf("%d/%d replicas ready", live.Status.ReadyReplicas, rt.Spec.Replicas))
		} else {
			rt.Status.Phase = "Progressing"
			setRetrieverCond(&rt, ragv1alpha1.ConditionAvailable, metav1.ConditionFalse, "VectorIndexUnhealthy",
				viReason)
		}
	} else {
		rt.Status.Phase = "Progressing"
		setRetrieverCond(&rt, ragv1alpha1.ConditionAvailable, metav1.ConditionFalse, "Progressing",
			fmt.Sprintf("%d/%d replicas ready", live.Status.ReadyReplicas, rt.Spec.Replicas))
	}
	return ctrl.Result{}, r.Status().Update(ctx, &rt)
}

func (r *RetrieverReconciler) computeSecretsHash(ctx context.Context, rt *ragv1alpha1.Retriever, kb *ragv1alpha1.KnowledgeBase) string {
	hasher := sha256.New()

	appendSecretHash(ctx, r.Client, kb.Namespace, "vectorStore.credentials", kb.Spec.VectorStore.CredentialsSecretRef, hasher)
	appendSecretHash(ctx, r.Client, kb.Namespace, "embedding.apiKey", kb.Spec.Embedding.APIKeySecretRef, hasher)

	if rt.Spec.Generation != nil && rt.Spec.Generation.APIKeySecretRef != nil {
		appendSecretHash(ctx, r.Client, rt.Namespace, "generation.apiKey", rt.Spec.Generation.APIKeySecretRef, hasher)
	}

	return hex.EncodeToString(hasher.Sum(nil))
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

func intPtrVal(i *int, def int) int {
	if i == nil {
		return def
	}
	return *i
}

func (r *RetrieverReconciler) desiredDeployment(rt *ragv1alpha1.Retriever, kb *ragv1alpha1.KnowledgeBase, secretHash string) *appsv1.Deployment {
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
	rerankCandidates := 0
	if rt.Spec.Rerank != nil {
		rerankModel = rt.Spec.Rerank.Model
		rerankCandidates = rt.Spec.Rerank.CandidatePoolSize
	}

	emb := kb.Spec.Embedding
	// Use the model actually in the store, not the desired spec — the index
	// may still contain data from a previous model while a re-ingest is pending.
	activeModel := emb.Model
	if kb.Status.ObservedEmbeddingModel != "" {
		activeModel = kb.Status.ObservedEmbeddingModel
	}
	env := []corev1.EnvVar{
		{Name: "VECTORSTORE_TYPE", Value: string(kb.Spec.VectorStore.Type)},
		{Name: "VECTORSTORE_ENDPOINT", Value: kb.Spec.VectorStore.Endpoint},
		{Name: "VECTORSTORE_COLLECTION", Value: collection},
		{Name: "DISTANCE", Value: string(kb.Spec.VectorStore.Distance)},
		{Name: "EMBEDDING_MODEL", Value: activeModel},
		// Query embedding must use the same provider as ingestion.
		{Name: "EMBEDDING_PROVIDER", Value: emb.Provider},
		{Name: "EMBEDDING_BASE_URL", Value: emb.BaseURL},
		{Name: "EMBEDDING_DIMENSION", Value: fmt.Sprintf("%d", emb.Dimension)},
		{Name: "EMBEDDING_QUERY_PREFIX", Value: emb.QueryPrefix},
		{Name: "EMBEDDING_DOC_PREFIX", Value: emb.DocumentPrefix},
		{Name: "TOPK", Value: fmt.Sprintf("%d", defaultInt(rt.Spec.TopK, 8))},
		{Name: "SCORE_THRESHOLD", Value: fmt.Sprintf("%d", rt.Spec.ScoreThresholdPercent)},
		{Name: "RERANK_ENABLED", Value: fmt.Sprintf("%t", rerankEnabled)},
		{Name: "RERANK_MODEL", Value: rerankModel},
		{Name: "RERANK_CANDIDATES", Value: fmt.Sprintf("%d", rerankCandidates)},
		{Name: "HYBRID_DEFAULT", Value: fmt.Sprintf("%t", rt.Spec.Hybrid)},
		{Name: "HYBRID_DENSE_PERCENT", Value: fmt.Sprintf("%d", intPtrVal(rt.Spec.HybridDensePercent, 50))},
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

	env = append(env, scratchEnv()...)
	// Disable playground ingest endpoints in production deployments.
	env = append(env, corev1.EnvVar{Name: "DISABLE_PLAYGROUND_INGEST", Value: "true"})

	var resources corev1.ResourceRequirements
	if rt.Spec.Resources != nil {
		resources = *rt.Spec.Resources
	} else {
		resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
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
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"checksum/secrets": secretHash,
					},
				},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken:  ptr.To(false),
					PriorityClassName:             "kuberag-system",
					TerminationGracePeriodSeconds: ptr.To(int64(60)),
					SecurityContext:               hardenedPodSecurityContext(),
					Volumes:                       []corev1.Volume{scratchVolume()},
					NodeSelector:                  rt.Spec.NodeSelector,
					Tolerations:                   rt.Spec.Tolerations,
					Affinity:                      rt.Spec.Affinity,
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
						{
							MaxSkew:           1,
							TopologyKey:       "topology.kubernetes.io/zone",
							WhenUnsatisfiable: corev1.ScheduleAnyway,
							LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "retriever",
							Image:           retrieverImage(rt),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports:           []corev1.ContainerPort{{ContainerPort: 8000, Name: "http"}},
							Env:             env,
							Resources:       resources,
							SecurityContext: hardenedContainerSecurityContext(),
							VolumeMounts:    []corev1.VolumeMount{scratchMount()},
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
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt32(8000),
									},
								},
								InitialDelaySeconds: 60,
								PeriodSeconds:       30,
							},
							StartupProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt32(8000),
									},
								},
								FailureThreshold: 18,
								PeriodSeconds:    10,
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
		Watches(
			&ragv1alpha1.KnowledgeBase{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				kb, ok := obj.(*ragv1alpha1.KnowledgeBase)
				if !ok {
					return nil
				}
				var list ragv1alpha1.RetrieverList
				if err := r.List(ctx, &list, client.InNamespace(kb.Namespace)); err != nil {
					return nil
				}
				var reqs []reconcile.Request
				for _, rt := range list.Items {
					if rt.Spec.KnowledgeBaseRef.Name == kb.Name {
						reqs = append(reqs, reconcile.Request{
							NamespacedName: types.NamespacedName{
								Namespace: rt.Namespace,
								Name:      rt.Name,
							},
						})
					}
				}
				return reqs
			}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				secret, ok := obj.(*corev1.Secret)
				if !ok {
					return nil
				}
				var list ragv1alpha1.RetrieverList
				if err := r.List(ctx, &list, client.InNamespace(secret.Namespace)); err != nil {
					return nil
				}
				var reqs []reconcile.Request
				for _, rt := range list.Items {
					var kb ragv1alpha1.KnowledgeBase
					kbKey := types.NamespacedName{Namespace: rt.Namespace, Name: rt.Spec.KnowledgeBaseRef.Name}
					if err := r.Get(ctx, kbKey, &kb); err == nil {
						referenced := false
						if kb.Spec.VectorStore.CredentialsSecretRef != nil && kb.Spec.VectorStore.CredentialsSecretRef.Name == secret.Name {
							referenced = true
						}
						if kb.Spec.Embedding.APIKeySecretRef != nil && kb.Spec.Embedding.APIKeySecretRef.Name == secret.Name {
							referenced = true
						}
						if rt.Spec.Generation != nil && rt.Spec.Generation.APIKeySecretRef != nil && rt.Spec.Generation.APIKeySecretRef.Name == secret.Name {
							referenced = true
						}
						if referenced {
							reqs = append(reqs, reconcile.Request{
								NamespacedName: types.NamespacedName{
									Namespace: rt.Namespace,
									Name:      rt.Name,
								},
							})
						}
					}
				}
				return reqs
			}),
		).
		Complete(r)
}
