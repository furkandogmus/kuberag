package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
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
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *RetrieverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rt ragv1alpha1.Retriever
	if err := r.Get(ctx, req.NamespacedName, &rt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tracer := otel.Tracer("kuberag")
	ctx, span := tracer.Start(ctx, "Retriever.Reconcile",
		trace.WithAttributes(
			attribute.String("retriever.name", rt.Name),
			attribute.String("retriever.namespace", rt.Namespace),
		))
	defer span.End()

	// Resolve the referenced KnowledgeBase to wire store/model into the server.
	var kb ragv1alpha1.KnowledgeBase
	kbKey := types.NamespacedName{Namespace: rt.Namespace, Name: rt.Spec.KnowledgeBaseRef.Name}
	if err := r.Get(ctx, kbKey, &kb); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.removeRetrieverWorkload(ctx, &rt); err != nil {
				return ctrl.Result{}, err
			}
			rt.Status.Phase = "Pending"
			rt.Status.ReadyReplicas = 0
			rt.Status.ServiceEndpoint = ""
			rt.Status.PublicEndpoint = ""
			rt.Status.ObservedGeneration = rt.Generation
			setRetrieverCond(&rt, ragv1alpha1.ConditionAvailable, metav1.ConditionFalse,
				"KnowledgeBaseNotFound", "referenced KnowledgeBase not found")
			return ctrl.Result{}, r.Status().Update(ctx, &rt)
		}
		return ctrl.Result{}, err
	}

	if reason, message := r.validateRetrieverSecrets(ctx, &rt, &kb); reason != "" {
		if err := r.removeRetrieverWorkload(ctx, &rt); err != nil {
			return ctrl.Result{}, err
		}
		rt.Status.Phase = "Pending"
		rt.Status.ReadyReplicas = 0
		rt.Status.ServiceEndpoint = ""
		rt.Status.PublicEndpoint = ""
		rt.Status.ObservedGeneration = rt.Generation
		setRetrieverCond(&rt, ragv1alpha1.ConditionAvailable, metav1.ConditionFalse, reason, message)
		return ctrl.Result{}, r.Status().Update(ctx, &rt)
	}

	secretHash := r.computeSecretsHash(ctx, &rt, &kb)
	dep := r.desiredDeployment(&rt, &kb, secretHash)
	if err := controllerutil.SetControllerReference(&rt, dep, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyDeployment(ctx, dep, autoscalingEnabled(&rt)); err != nil {
		return ctrl.Result{}, err
	}

	svc := r.desiredService(&rt)
	if err := controllerutil.SetControllerReference(&rt, svc, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyService(ctx, svc); err != nil {
		return ctrl.Result{}, err
	}
	metricsSvc := r.desiredMetricsService(&rt)
	if err := controllerutil.SetControllerReference(&rt, metricsSvc, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyService(ctx, metricsSvc); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileIngress(ctx, &rt); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileOIDCNetworkPolicy(ctx, &rt); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcilePodDisruptionBudget(ctx, &rt); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileHorizontalPodAutoscaler(ctx, &rt); err != nil {
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
	rt.Status.PublicEndpoint = ""
	if rt.Spec.Ingress != nil {
		scheme := "http"
		if rt.Spec.Ingress.TLSSecretName != "" {
			scheme = "https"
		}
		path := rt.Spec.Ingress.Path
		if path == "" {
			path = "/"
		}
		rt.Status.PublicEndpoint = fmt.Sprintf("%s://%s%s", scheme, rt.Spec.Ingress.Host, path)
	}

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

	minReady := desiredMinimumReplicas(&rt)
	if live.Status.ReadyReplicas >= minReady && minReady > 0 {
		if viHealthy {
			rt.Status.Phase = "Available"
			setRetrieverCond(&rt, ragv1alpha1.ConditionAvailable, metav1.ConditionTrue, "MinimumReplicasAvailable",
				fmt.Sprintf("%d/%d replicas ready", live.Status.ReadyReplicas, minReady))
		} else {
			rt.Status.Phase = "Progressing"
			setRetrieverCond(&rt, ragv1alpha1.ConditionAvailable, metav1.ConditionFalse, "VectorIndexUnhealthy",
				viReason)
		}
	} else {
		rt.Status.Phase = "Progressing"
		setRetrieverCond(&rt, ragv1alpha1.ConditionAvailable, metav1.ConditionFalse, "Progressing",
			fmt.Sprintf("%d/%d replicas ready", live.Status.ReadyReplicas, minReady))
	}
	return ctrl.Result{}, r.Status().Update(ctx, &rt)
}

func (r *RetrieverReconciler) computeSecretsHash(ctx context.Context, rt *ragv1alpha1.Retriever, kb *ragv1alpha1.KnowledgeBase) string {
	hasher := sha256.New()

	appendSecretHash(ctx, r.Client, kb.Namespace, "vectorStore.credentials", kb.Spec.VectorStore.CredentialsSecretRef, hasher)
	appendSecretHash(ctx, r.Client, kb.Namespace, "embedding.apiKey", kb.Spec.Embedding.APIKeySecretRef, hasher)

	if rt.Spec.APIKeySecretRef != nil {
		appendSecretHash(ctx, r.Client, rt.Namespace, "retriever.apiKey", rt.Spec.APIKeySecretRef, hasher)
	}
	if rt.Spec.RateLimit != nil &&
		rt.Spec.RateLimit.Backend == "redis" &&
		rt.Spec.RateLimit.RedisURLSecretRef != nil {
		appendSecretHash(ctx, r.Client, rt.Namespace, "rateLimit.redisURL", rt.Spec.RateLimit.RedisURLSecretRef, hasher)
	}
	if rt.Spec.OIDC != nil {
		appendSecretHash(ctx, r.Client, rt.Namespace, "oidc.clientID", &rt.Spec.OIDC.ClientIDSecretRef, hasher)
		appendSecretHash(ctx, r.Client, rt.Namespace, "oidc.clientSecret", &rt.Spec.OIDC.ClientSecretSecretRef, hasher)
		appendSecretHash(ctx, r.Client, rt.Namespace, "oidc.cookieSecret", &rt.Spec.OIDC.CookieSecretRef, hasher)
	}
	if rt.Spec.Generation != nil && rt.Spec.Generation.APIKeySecretRef != nil {
		appendSecretHash(ctx, r.Client, rt.Namespace, "generation.apiKey", rt.Spec.Generation.APIKeySecretRef, hasher)
	}

	return hex.EncodeToString(hasher.Sum(nil)[:4])
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

func autoscalingEnabled(rt *ragv1alpha1.Retriever) bool {
	return rt.Spec.Autoscaling != nil && boolPtrVal(rt.Spec.Autoscaling.Enabled, false)
}

func oidcEnabled(rt *ragv1alpha1.Retriever) bool {
	return rt.Spec.OIDC != nil
}

func desiredMinimumReplicas(rt *ragv1alpha1.Retriever) int32 {
	if autoscalingEnabled(rt) {
		return defaultInt32(rt.Spec.Autoscaling.MinReplicas, 2)
	}
	return rt.Spec.Replicas
}

func defaultInt32(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

func defaultInt64(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

func (r *RetrieverReconciler) validateRetrieverSecrets(ctx context.Context, rt *ragv1alpha1.Retriever, kb *ragv1alpha1.KnowledgeBase) (string, string) {
	refs := []struct {
		label string
		ref   *ragv1alpha1.SecretKeyRef
	}{
		{"vector store credentials", kb.Spec.VectorStore.CredentialsSecretRef},
		{"embedding API key", kb.Spec.Embedding.APIKeySecretRef},
		{"retriever API key", rt.Spec.APIKeySecretRef},
	}
	if rt.Spec.RateLimit != nil && rt.Spec.RateLimit.Backend == "redis" {
		refs = append(refs, struct {
			label string
			ref   *ragv1alpha1.SecretKeyRef
		}{"rate-limit Redis URL", rt.Spec.RateLimit.RedisURLSecretRef})
	}
	if rt.Spec.Generation != nil {
		refs = append(refs, struct {
			label string
			ref   *ragv1alpha1.SecretKeyRef
		}{"generation API key", rt.Spec.Generation.APIKeySecretRef})
	}
	if rt.Spec.OIDC != nil {
		refs = append(refs,
			struct {
				label string
				ref   *ragv1alpha1.SecretKeyRef
			}{"OIDC client ID", &rt.Spec.OIDC.ClientIDSecretRef},
			struct {
				label string
				ref   *ragv1alpha1.SecretKeyRef
			}{"OIDC client secret", &rt.Spec.OIDC.ClientSecretSecretRef},
			struct {
				label string
				ref   *ragv1alpha1.SecretKeyRef
			}{"OIDC cookie secret", &rt.Spec.OIDC.CookieSecretRef},
		)
	}

	for _, item := range refs {
		if item.ref == nil {
			continue
		}
		var secret corev1.Secret
		key := types.NamespacedName{Namespace: rt.Namespace, Name: item.ref.Name}
		if err := r.Get(ctx, key, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return "SecretNotFound", fmt.Sprintf("%s Secret %q not found", item.label, item.ref.Name)
			}
			return "SecretReadFailed", fmt.Sprintf("cannot read %s Secret %q: %v", item.label, item.ref.Name, err)
		}
		value, ok := secret.Data[item.ref.Key]
		if !ok {
			return "SecretKeyNotFound", fmt.Sprintf("%s Secret %q does not contain key %q", item.label, item.ref.Name, item.ref.Key)
		}
		if len(value) == 0 {
			return "SecretKeyEmpty", fmt.Sprintf("%s Secret %q key %q is empty", item.label, item.ref.Name, item.ref.Key)
		}
	}
	return "", ""
}

func (r *RetrieverReconciler) removeRetrieverWorkload(ctx context.Context, rt *ragv1alpha1.Retriever) error {
	objects := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: rt.Name + "-retriever", Namespace: rt.Namespace}},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: rt.Name + "-retriever", Namespace: rt.Namespace}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: rt.Name + "-retriever", Namespace: rt.Namespace}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: rt.Name + "-retriever", Namespace: rt.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: rt.Name + "-retriever-oidc", Namespace: rt.Namespace}},
	}
	for _, obj := range objects {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *RetrieverReconciler) desiredDeployment(rt *ragv1alpha1.Retriever, kb *ragv1alpha1.KnowledgeBase, secretHash string) *appsv1.Deployment {
	labels := map[string]string{
		labelManagedBy:             "kuberag",
		"rag.furkan.dev/retriever": rt.Name,
	}
	replicas := desiredMinimumReplicas(rt)
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
	// Use the embedding config actually in the store so queries match indexed data.
	// When the spec changes (model, provider, baseURL, prefixes), the index still
	// contains data from the previous config until re-ingestion completes.
	if kb.Status.ObservedEmbedding != nil {
		emb = *kb.Status.ObservedEmbedding
	} else if kb.Status.ObservedEmbeddingModel != "" {
		emb.Model = kb.Status.ObservedEmbeddingModel
	}
	env := []corev1.EnvVar{
		{Name: "VECTORSTORE_TYPE", Value: string(kb.Spec.VectorStore.Type)},
		{Name: "VECTORSTORE_ENDPOINT", Value: kb.Spec.VectorStore.Endpoint},
		{Name: "VECTORSTORE_COLLECTION", Value: collection},
		{Name: "DISTANCE", Value: string(kb.Spec.VectorStore.Distance)},
		{Name: "EMBEDDING_MODEL", Value: emb.Model},
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
		{Name: "MAX_CONCURRENT_REQUESTS", Value: fmt.Sprintf("%d", defaultInt(rt.Spec.MaxConcurrentRequests, 32))},
		{Name: "MAX_REQUEST_BODY_BYTES", Value: fmt.Sprintf("%d", defaultInt64(rt.Spec.MaxRequestBodyBytes, 1048576))},
	}
	if rate := rt.Spec.RateLimit; rate != nil && boolPtrVal(rate.Enabled, true) {
		backend := rate.Backend
		if backend == "" {
			backend = "local"
		}
		keyPrefix := rate.RedisKeyPrefix
		if keyPrefix == "" {
			keyPrefix = "kuberag:ratelimit"
		}
		env = append(env,
			corev1.EnvVar{Name: "RATE_LIMIT_ENABLED", Value: "true"},
			corev1.EnvVar{Name: "RATE_LIMIT_REQUESTS_PER_MINUTE", Value: fmt.Sprintf("%d", defaultInt(rate.RequestsPerMinute, 60))},
			corev1.EnvVar{Name: "RATE_LIMIT_BURST", Value: fmt.Sprintf("%d", defaultInt(rate.Burst, 20))},
			corev1.EnvVar{Name: "RATE_LIMIT_BACKEND", Value: backend},
			corev1.EnvVar{Name: "RATE_LIMIT_REDIS_KEY_PREFIX", Value: keyPrefix},
			corev1.EnvVar{Name: "RATE_LIMIT_CLIENT_ID_HEADER", Value: rate.ClientIdentityHeader},
		)
		if backend == "redis" && rate.RedisURLSecretRef != nil {
			env = append(env, secretEnv("RATE_LIMIT_REDIS_URL", rate.RedisURLSecretRef))
		}
	}
	if kb.Spec.VectorStore.CredentialsSecretRef != nil {
		env = append(env, secretEnv("VECTORSTORE_CREDENTIAL", kb.Spec.VectorStore.CredentialsSecretRef))
	}
	if emb.APIKeySecretRef != nil {
		env = append(env, secretEnv("EMBEDDING_API_KEY", emb.APIKeySecretRef))
	}
	if rt.Spec.APIKeySecretRef != nil {
		env = append(env,
			corev1.EnvVar{Name: "RETRIEVER_AUTH_ENABLED", Value: "true"},
			secretEnv("RETRIEVER_API_KEY", rt.Spec.APIKeySecretRef),
		)
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
	env = append(env,
		corev1.EnvVar{Name: "DISABLE_PLAYGROUND_INGEST", Value: "true"},
		corev1.EnvVar{Name: "METRICS_ENABLED", Value: "true"},
		corev1.EnvVar{Name: "METRICS_PORT", Value: "9090"},
	)

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

	containers := []corev1.Container{
		{
			Name:            "retriever",
			Image:           retrieverImage(rt),
			ImagePullPolicy: corev1.PullIfNotPresent,
			Ports: []corev1.ContainerPort{
				{ContainerPort: 8000, Name: "http"},
				{ContainerPort: 9090, Name: "metrics"},
			},
			Env:             env,
			Resources:       resources,
			SecurityContext: hardenedContainerSecurityContext(),
			VolumeMounts:    []corev1.VolumeMount{scratchMount()},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/readyz",
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
	}
	if oidc := rt.Spec.OIDC; oidc != nil {
		emailDomains := oidc.EmailDomains
		if len(emailDomains) == 0 {
			emailDomains = []string{"*"}
		}
		scheme := "http"
		if rt.Spec.Ingress != nil && rt.Spec.Ingress.TLSSecretName != "" {
			scheme = "https"
		}
		args := []string{
			"--provider=oidc",
			"--oidc-issuer-url=" + oidc.IssuerURL,
			"--http-address=0.0.0.0:4180",
			"--upstream=http://127.0.0.1:8000",
			"--redirect-url=" + scheme + "://" + rt.Spec.Ingress.Host + "/oauth2/callback",
			"--reverse-proxy=true",
			"--skip-provider-button=true",
			"--code-challenge-method=S256",
			"--cookie-name=_kuberag_oauth2_proxy",
			"--cookie-samesite=lax",
			fmt.Sprintf("--cookie-secure=%t", scheme == "https"),
		}
		for _, domain := range emailDomains {
			args = append(args, "--email-domain="+domain)
		}
		for _, group := range oidc.Groups {
			args = append(args, "--allowed-group="+group)
		}
		image := oidc.Image
		if image == "" {
			image = "quay.io/oauth2-proxy/oauth2-proxy:v7.15.3"
		}
		containers = append(containers, corev1.Container{
			Name:            "oauth2-proxy",
			Image:           image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Args:            args,
			Ports:           []corev1.ContainerPort{{ContainerPort: 4180, Name: "proxy"}},
			Env: []corev1.EnvVar{
				secretEnv("OAUTH2_PROXY_CLIENT_ID", &oidc.ClientIDSecretRef),
				secretEnv("OAUTH2_PROXY_CLIENT_SECRET", &oidc.ClientSecretSecretRef),
				secretEnv("OAUTH2_PROXY_COOKIE_SECRET", &oidc.CookieSecretRef),
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("25m"),
					corev1.ResourceMemory: resource.MustParse("32Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("250m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
			SecurityContext: hardenedContainerSecurityContext(),
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/ping", Port: intstr.FromInt32(4180)},
				},
				InitialDelaySeconds: 5,
				PeriodSeconds:       10,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/ping", Port: intstr.FromInt32(4180)},
				},
				InitialDelaySeconds: 15,
				PeriodSeconds:       20,
			},
		})
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
					Volumes:                       []corev1.Volume{scratchVolume("")},
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
					Containers: containers,
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
	targetPort := intstr.FromInt32(8000)
	if oidcEnabled(rt) {
		targetPort = intstr.FromInt32(4180)
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
				{Name: "http", Port: 8000, TargetPort: targetPort},
			},
		},
	}
}

func (r *RetrieverReconciler) desiredMetricsService(rt *ragv1alpha1.Retriever) *corev1.Service {
	selector := map[string]string{
		labelManagedBy:             "kuberag",
		"rag.furkan.dev/retriever": rt.Name,
	}
	labels := map[string]string{
		labelManagedBy:                "kuberag",
		"app.kubernetes.io/component": "retriever-metrics",
		"rag.furkan.dev/retriever":    rt.Name,
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rt.Name + "-retriever-metrics",
			Namespace: rt.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"prometheus.io/scrape": "true",
				"prometheus.io/port":   "9090",
				"prometheus.io/path":   "/metrics",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports: []corev1.ServicePort{{
				Name:       "metrics",
				Port:       9090,
				TargetPort: intstr.FromInt32(9090),
			}},
		},
	}
}

func (r *RetrieverReconciler) reconcileIngress(ctx context.Context, rt *ragv1alpha1.Retriever) error {
	key := types.NamespacedName{Namespace: rt.Namespace, Name: rt.Name + "-retriever"}
	if rt.Spec.Ingress == nil {
		return client.IgnoreNotFound(r.Delete(ctx, &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		}))
	}

	path := rt.Spec.Ingress.Path
	if path == "" {
		path = "/"
	}
	pathType := networkingv1.PathTypePrefix
	annotations := make(map[string]string, len(rt.Spec.Ingress.Annotations)+1)
	for k, v := range rt.Spec.Ingress.Annotations {
		annotations[k] = v
	}
	if rt.Spec.Ingress.ClusterIssuer != "" {
		annotations["cert-manager.io/cluster-issuer"] = rt.Spec.Ingress.ClusterIssuer
	}
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        key.Name,
			Namespace:   key.Namespace,
			Annotations: annotations,
			Labels: map[string]string{
				labelManagedBy:             "kuberag",
				"rag.furkan.dev/retriever": rt.Name,
			},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: rt.Spec.Ingress.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     path,
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: rt.Name + "-retriever",
									Port: networkingv1.ServiceBackendPort{Number: 8000},
								},
							},
						}},
					},
				},
			}},
		},
	}
	if rt.Spec.Ingress.ClassName != "" {
		ingress.Spec.IngressClassName = ptr.To(rt.Spec.Ingress.ClassName)
	}
	if rt.Spec.Ingress.TLSSecretName != "" {
		ingress.Spec.TLS = []networkingv1.IngressTLS{{
			Hosts:      []string{rt.Spec.Ingress.Host},
			SecretName: rt.Spec.Ingress.TLSSecretName,
		}}
	}
	if err := controllerutil.SetControllerReference(rt, ingress, r.Scheme); err != nil {
		return err
	}

	var existing networkingv1.Ingress
	if err := r.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
		return r.Create(ctx, ingress)
	} else if err != nil {
		return err
	}
	existing.Labels = ingress.Labels
	existing.Annotations = ingress.Annotations
	existing.Spec = ingress.Spec
	return r.Update(ctx, &existing)
}

func (r *RetrieverReconciler) reconcileOIDCNetworkPolicy(ctx context.Context, rt *ragv1alpha1.Retriever) error {
	key := types.NamespacedName{Namespace: rt.Namespace, Name: rt.Name + "-retriever-oidc"}
	if !oidcEnabled(rt) {
		return client.IgnoreNotFound(r.Delete(ctx, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		}))
	}

	labels := map[string]string{
		labelManagedBy:             "kuberag",
		"rag.furkan.dev/retriever": rt.Name,
	}
	proxyPort := intstr.FromInt32(4180)
	metricsPort := intstr.FromInt32(9090)
	protocol := corev1.ProtocolTCP
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{
					Protocol: &protocol,
					Port:     &proxyPort,
				}, {
					Protocol: &protocol,
					Port:     &metricsPort,
				}},
			}},
		},
	}
	if err := controllerutil.SetControllerReference(rt, policy, r.Scheme); err != nil {
		return err
	}

	var existing networkingv1.NetworkPolicy
	if err := r.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
		return r.Create(ctx, policy)
	} else if err != nil {
		return err
	}
	existing.Labels = policy.Labels
	existing.Spec = policy.Spec
	return r.Update(ctx, &existing)
}

func (r *RetrieverReconciler) applyDeployment(ctx context.Context, dep *appsv1.Deployment, preserveReplicas bool) error {
	var existing appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, dep)
	}
	if err != nil {
		return err
	}
	if preserveReplicas {
		dep.Spec.Replicas = existing.Spec.Replicas
	}
	existing.Spec = dep.Spec
	existing.Labels = dep.Labels
	return r.Update(ctx, &existing)
}

func (r *RetrieverReconciler) reconcilePodDisruptionBudget(ctx context.Context, rt *ragv1alpha1.Retriever) error {
	enabled := rt.Spec.PodDisruptionBudget == nil && desiredMinimumReplicas(rt) > 1
	if rt.Spec.PodDisruptionBudget != nil {
		enabled = *rt.Spec.PodDisruptionBudget
	}
	key := types.NamespacedName{Namespace: rt.Namespace, Name: rt.Name + "-retriever"}
	if !enabled {
		return client.IgnoreNotFound(r.Delete(ctx, &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		}))
	}

	labels := map[string]string{
		labelManagedBy:             "kuberag",
		"rag.furkan.dev/retriever": rt.Name,
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr.To(intstr.FromInt32(1)),
			Selector:     &metav1.LabelSelector{MatchLabels: labels},
		},
	}
	if err := controllerutil.SetControllerReference(rt, pdb, r.Scheme); err != nil {
		return err
	}

	var existing policyv1.PodDisruptionBudget
	if err := r.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
		return r.Create(ctx, pdb)
	} else if err != nil {
		return err
	}
	existing.Labels = pdb.Labels
	existing.Spec = pdb.Spec
	return r.Update(ctx, &existing)
}

func (r *RetrieverReconciler) reconcileHorizontalPodAutoscaler(ctx context.Context, rt *ragv1alpha1.Retriever) error {
	key := types.NamespacedName{Namespace: rt.Namespace, Name: rt.Name + "-retriever"}
	if !autoscalingEnabled(rt) {
		return client.IgnoreNotFound(r.Delete(ctx, &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		}))
	}

	minReplicas := defaultInt32(rt.Spec.Autoscaling.MinReplicas, 2)
	maxReplicas := defaultInt32(rt.Spec.Autoscaling.MaxReplicas, 10)
	targetCPU := defaultInt32(rt.Spec.Autoscaling.TargetCPUUtilizationPercentage, 70)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				labelManagedBy:             "kuberag",
				"rag.furkan.dev/retriever": rt.Name,
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       rt.Name + "-retriever",
			},
			MinReplicas: &minReplicas,
			MaxReplicas: maxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &targetCPU,
					},
				},
			}},
		},
	}
	if err := controllerutil.SetControllerReference(rt, hpa, r.Scheme); err != nil {
		return err
	}

	var existing autoscalingv2.HorizontalPodAutoscaler
	if err := r.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
		return r.Create(ctx, hpa)
	} else if err != nil {
		return err
	}
	existing.Labels = hpa.Labels
	existing.Spec = hpa.Spec
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
	existing.Labels = svc.Labels
	existing.Annotations = svc.Annotations
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
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&networkingv1.NetworkPolicy{}).
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
					referenced := rt.Spec.APIKeySecretRef != nil && rt.Spec.APIKeySecretRef.Name == secret.Name
					if rt.Spec.RateLimit != nil &&
						rt.Spec.RateLimit.Backend == "redis" &&
						rt.Spec.RateLimit.RedisURLSecretRef != nil &&
						rt.Spec.RateLimit.RedisURLSecretRef.Name == secret.Name {
						referenced = true
					}
					if rt.Spec.Generation != nil && rt.Spec.Generation.APIKeySecretRef != nil && rt.Spec.Generation.APIKeySecretRef.Name == secret.Name {
						referenced = true
					}
					if rt.Spec.OIDC != nil &&
						(rt.Spec.OIDC.ClientIDSecretRef.Name == secret.Name ||
							rt.Spec.OIDC.ClientSecretSecretRef.Name == secret.Name ||
							rt.Spec.OIDC.CookieSecretRef.Name == secret.Name) {
						referenced = true
					}
					var kb ragv1alpha1.KnowledgeBase
					kbKey := types.NamespacedName{Namespace: rt.Namespace, Name: rt.Spec.KnowledgeBaseRef.Name}
					if err := r.Get(ctx, kbKey, &kb); err == nil {
						if kb.Spec.VectorStore.CredentialsSecretRef != nil && kb.Spec.VectorStore.CredentialsSecretRef.Name == secret.Name {
							referenced = true
						}
						if kb.Spec.Embedding.APIKeySecretRef != nil && kb.Spec.Embedding.APIKeySecretRef.Name == secret.Name {
							referenced = true
						}
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
				return reqs
			}),
		).
		Complete(r)
}
