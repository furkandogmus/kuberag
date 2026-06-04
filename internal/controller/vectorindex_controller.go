package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

// VectorIndexReconciler probes the health of a vector store collection.
type VectorIndexReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// HTTP lets tests inject a fake client.
	HTTP *http.Client
}

// +kubebuilder:rbac:groups=rag.furkan.dev,resources=vectorindices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rag.furkan.dev,resources=vectorindices/status,verbs=get;update;patch

func (r *VectorIndexReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var vi ragv1alpha1.VectorIndex
	if err := r.Get(ctx, req.NamespacedName, &vi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	probe := r.probe(ctx, &vi)

	vi.Status.Health = probe.health
	vi.Status.PointCount = probe.points
	vi.Status.Dimension = probe.dimension
	vi.Status.Message = probe.message
	now := metav1.Now()
	vi.Status.LastProbeTime = &now

	condStatus := metav1.ConditionTrue
	if probe.health != ragv1alpha1.IndexHealthy {
		condStatus = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&vi.Status.Conditions, metav1.Condition{
		Type:               ragv1alpha1.ConditionReady,
		Status:             condStatus,
		Reason:             string(probe.health),
		Message:            probe.message,
		ObservedGeneration: vi.Generation,
	})

	if err := r.Status().Update(ctx, &vi); err != nil {
		return ctrl.Result{}, err
	}

	interval := vi.Spec.ProbeIntervalSeconds
	if interval < 10 {
		interval = 60
	}
	return ctrl.Result{RequeueAfter: time.Duration(interval) * time.Second}, nil
}

type probeResult struct {
	health    ragv1alpha1.VectorIndexHealth
	points    int64
	dimension int
	message   string
}

func (r *VectorIndexReconciler) probe(ctx context.Context, vi *ragv1alpha1.VectorIndex) probeResult {
	switch vi.Spec.Store.Type {
	case ragv1alpha1.VectorStoreQdrant:
		return r.probeQdrant(ctx, vi)
	default:
		return probeResult{
			health:  ragv1alpha1.IndexUnknown,
			message: fmt.Sprintf("health probing not implemented for store type %q; relies on ingestion success", vi.Spec.Store.Type),
		}
	}
}

// qdrantCollectionResponse mirrors the subset of Qdrant's collection info we read.
type qdrantCollectionResponse struct {
	Result struct {
		PointsCount int64 `json:"points_count"`
		Config      struct {
			Params struct {
				Vectors struct {
					Size int `json:"size"`
				} `json:"vectors"`
			} `json:"params"`
		} `json:"config"`
	} `json:"result"`
	Status string `json:"status"`
}

func (r *VectorIndexReconciler) probeQdrant(ctx context.Context, vi *ragv1alpha1.VectorIndex) probeResult {
	collection := vi.Spec.Store.Collection
	if collection == "" {
		collection = vi.Spec.KnowledgeBaseRef.Name
	}
	url := fmt.Sprintf("%s/collections/%s", vi.Spec.Store.Endpoint, collection)

	httpClient := r.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	reqCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return probeResult{health: ragv1alpha1.IndexUnknown, message: err.Error()}
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return probeResult{health: ragv1alpha1.IndexUnknown, message: fmt.Sprintf("probe error: %v", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return probeResult{health: ragv1alpha1.IndexMissing, message: "collection does not exist yet"}
	}
	if resp.StatusCode != http.StatusOK {
		return probeResult{health: ragv1alpha1.IndexDegraded, message: fmt.Sprintf("qdrant returned HTTP %d", resp.StatusCode)}
	}

	var body qdrantCollectionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return probeResult{health: ragv1alpha1.IndexUnknown, message: fmt.Sprintf("decode error: %v", err)}
	}

	dim := body.Result.Config.Params.Vectors.Size
	res := probeResult{
		health:    ragv1alpha1.IndexHealthy,
		points:    body.Result.PointsCount,
		dimension: dim,
		message:   fmt.Sprintf("collection status=%s", body.Status),
	}
	if vi.Spec.Dimension > 0 && dim > 0 && dim != vi.Spec.Dimension {
		res.health = ragv1alpha1.IndexDegraded
		res.message = fmt.Sprintf("dimension mismatch: store=%d expected=%d", dim, vi.Spec.Dimension)
	}
	return res
}

func (r *VectorIndexReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ragv1alpha1.VectorIndex{}).
		Complete(r)
}
