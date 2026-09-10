// ============================================================
// consumers.go: 가속기 소비자(Pod) 조회 — GET /api/v1/consumers
// 상세: 이 라인은 resource.k8s.io 를 링크하지 않으므로 귀속은 노드 단위 하나뿐이다.
// pod spec 에는 수량만 있고 어느 물리 카드인지는 kubelet 만 안다. 그 한계를
// attribution 으로 드러내고, 모르는 것을 추측해 채우지 않는다.
// 1.34 라인은 같은 라우트에 DRA 축(device·pending)이 더 붙는다.
// 생성일: 2026-08-07
// ============================================================
package apiserver

import (
	"net/http"
	"sort"

	corev1 "k8s.io/api/core/v1"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/intent"
)

// 귀속 등급. 이 라인에서 실제로 나오는 값은 node 하나다 — 나머지 둘은 1.34
// 라인과 응답 계약을 맞추기 위한 어휘이며, 화면이 라인마다 갈리지 않게 한다.
const (
	// AttributionDevice: 어느 장치인지 안다(DRA 할당 완료). 이 라인에서는 나오지 않는다.
	AttributionDevice = "device"
	// AttributionNode: 노드까지만 안다(device-plugin). 영영 알 수 없다.
	AttributionNode = "node"
	// AttributionPending: DRA claim 은 있으나 아직 미할당. 이 라인에서는 나오지 않는다.
	AttributionPending = "pending"
)

// ResourceRequest 는 Pod 가 요청한 가속기 리소스 하나다.
type ResourceRequest struct {
	Resource string `json:"resource"`
	Quantity int64  `json:"quantity"`
	Vendor   string `json:"vendor,omitempty"`
}

// ClaimRef 는 Pod 가 참조하는 ResourceClaim 하나다. 이 라인에서는 항상 비어 있다.
type ClaimRef struct {
	Name        string `json:"name"`
	DeviceClass string `json:"deviceClass,omitempty"`
	Status      string `json:"status"` // allocated | pending | missing
}

// ConsumerView 는 가속기를 요청한 Pod 하나다.
type ConsumerView struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node,omitempty"`
	Phase     string `json:"phase"`
	// AllocationAPI 는 이 라인에서 항상 npuv1alpha1.AllocationAPIDevicePlugin 이다.
	AllocationAPI string            `json:"allocationAPI"`
	Attribution   string            `json:"attribution"` // device | node | pending
	Requests      []ResourceRequest `json:"requests,omitempty"`
	// DeviceUIDs 는 Attribution == device 일 때만 채운다. 이 라인에서는 늘 비어 있고,
	// 빈 값은 "장치 없음" 이 아니라 "모른다" 는 뜻이다.
	DeviceUIDs []string   `json:"deviceUIDs,omitempty"`
	Claims     []ClaimRef `json:"claims,omitempty"`
}

// toConsumerView 는 Pod 하나를 소비자 뷰로 옮긴다. 두 번째 반환값이 false 면
// 가속기를 요청하지 않은 Pod 이므로 목록에서 뺀다.
func toConsumerView(pod corev1.Pod) (ConsumerView, bool) {
	v := ConsumerView{
		Namespace: pod.Namespace, Name: pod.Name,
		Node: pod.Spec.NodeName, Phase: string(pod.Status.Phase),
	}

	// 컨테이너 limits 를 합산한다. 가속기 리소스 판정은 intent.VendorForResource
	// 하나만 쓴다 — 규칙이 두 곳에 생기면 갈린다.
	totals := map[string]int64{}
	for i := range pod.Spec.Containers {
		for name, q := range pod.Spec.Containers[i].Resources.Limits {
			if intent.VendorForResource(string(name)) == "" {
				continue
			}
			totals[string(name)] += q.Value()
		}
	}
	if len(totals) == 0 {
		return ConsumerView{}, false
	}
	names := make([]string, 0, len(totals))
	for name := range totals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v.Requests = append(v.Requests, ResourceRequest{
			Resource: name, Quantity: totals[name], Vendor: intent.VendorForResource(name),
		})
	}
	v.AllocationAPI, v.Attribution = npuv1alpha1.AllocationAPIDevicePlugin, AttributionNode
	return v, true
}

// handleConsumers 는 가속기를 요청한 Pod 목록을 낸다.
// 클러스터 전체 Pod 를 흘리면 화면이 무관한 Pod 로 덮이므로 toConsumerView 가
// 걸러 낸 것만 담는다.
func (s *Server) handleConsumers(w http.ResponseWriter, r *http.Request) {
	var pods corev1.PodList
	if err := s.Reader.List(r.Context(), &pods); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]ConsumerView, 0, len(pods.Items))
	for i := range pods.Items {
		if v, ok := toConsumerView(pods.Items[i]); ok {
			out = append(out, v)
		}
	}
	if node := r.URL.Query().Get("node"); node != "" {
		out = filterSlice(out, func(v ConsumerView) bool { return v.Node == node })
	}
	if ns := r.URL.Query().Get("namespace"); ns != "" {
		out = filterSlice(out, func(v ConsumerView) bool { return v.Namespace == ns })
	}
	if vendor := r.URL.Query().Get("vendor"); vendor != "" {
		out = filterSlice(out, func(v ConsumerView) bool {
			for _, req := range v.Requests {
				if req.Vendor == vendor {
					return true
				}
			}
			return false
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	limit, offset := pageParams(r)
	writePage(w, out, limit, offset)
}
