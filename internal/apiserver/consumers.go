// ============================================================
// consumers.go: 가속기 소비자(Pod) 조회 — GET /api/v1/consumers
// 상세: 귀속 정밀도가 할당 API 마다 다르다. DRA 는 ResourceClaim 이 장치를 지목하므로
// 장치 단위로 알고, device-plugin 은 pod spec 에 수량만 있어 노드 단위까지만 안다.
// 그 차이를 attribution 으로 노출하고, 모르는 것은 추측해 채우지 않는다(fail-closed).
// AcceleratorView 에 consumers 를 달지 않는 이유도 같다 — 장치 행에 소비자를 달면
// device-plugin 경로에서 거짓 신호가 된다.
// 생성일: 2026-08-07
// ============================================================
package apiserver

import (
	"net/http"
	"sort"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/intent"
)

// 귀속 등급. 이 셋 외의 값을 만들지 않는다.
const (
	// AttributionDevice: 어느 장치인지 안다(DRA 할당 완료).
	AttributionDevice = "device"
	// AttributionNode: 노드까지만 안다(device-plugin). 영영 알 수 없다.
	AttributionNode = "node"
	// AttributionPending: DRA claim 은 있으나 아직 미할당. 곧 알게 된다.
	AttributionPending = "pending"
)

// ResourceRequest 는 Pod 가 요청한 가속기 리소스 하나다.
type ResourceRequest struct {
	Resource string `json:"resource"`
	Quantity int64  `json:"quantity"`
	Vendor   string `json:"vendor,omitempty"`
}

// ClaimRef 는 Pod 가 참조하는 ResourceClaim 하나다.
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
	// AllocationAPI 는 npuv1alpha1.AllocationAPIDevicePlugin 또는 AllocationAPIDRA 다.
	AllocationAPI string            `json:"allocationAPI"`
	Attribution   string            `json:"attribution"` // device | node | pending
	Requests      []ResourceRequest `json:"requests,omitempty"`
	// DeviceUIDs 는 Attribution == device 일 때만 채운다. 그 외에는 반드시 비운다 —
	// 빈 값은 "장치 없음" 이 아니라 "모른다" 는 뜻이고, UI 가 등급과 함께 읽는다.
	DeviceUIDs []string   `json:"deviceUIDs,omitempty"`
	Claims     []ClaimRef `json:"claims,omitempty"`
}

// toConsumerView 는 Pod 하나를 소비자 뷰로 옮긴다. 두 번째 반환값이 false 면
// 가속기를 요청하지 않은 Pod 이므로 목록에서 뺀다.
func toConsumerView(pod corev1.Pod, claims []resourcev1.ResourceClaim) (ConsumerView, bool) {
	v := ConsumerView{
		Namespace: pod.Namespace, Name: pod.Name,
		Node: pod.Spec.NodeName, Phase: string(pod.Status.Phase),
	}

	// device-plugin 축: 컨테이너 limits 를 합산한다. 가속기 리소스 판정은
	// intent.VendorForResource 하나만 쓴다 — 규칙이 두 곳에 생기면 갈린다.
	totals := map[string]int64{}
	for i := range pod.Spec.Containers {
		for name, q := range pod.Spec.Containers[i].Resources.Limits {
			if intent.VendorForResource(string(name)) == "" {
				continue
			}
			totals[string(name)] += q.Value()
		}
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

	// DRA 축: pod 가 참조하는 claim 을 찾아 할당 결과를 읽는다.
	byName := map[string]*resourcev1.ResourceClaim{}
	for i := range claims {
		if claims[i].Namespace == pod.Namespace {
			byName[claims[i].Name] = &claims[i]
		}
	}
	allocated := false
	for _, prc := range pod.Spec.ResourceClaims {
		name := ""
		if prc.ResourceClaimName != nil {
			name = *prc.ResourceClaimName
		}
		ref := ClaimRef{Name: name, Status: "missing"}
		if c := byName[name]; c != nil {
			ref.DeviceClass = deviceClassOf(c)
			if c.Status.Allocation != nil {
				ref.Status = "allocated"
				allocated = true
				for _, res := range c.Status.Allocation.Devices.Results {
					v.DeviceUIDs = append(v.DeviceUIDs, res.Device)
				}
			} else {
				ref.Status = "pending"
			}
		}
		v.Claims = append(v.Claims, ref)
	}

	switch {
	case len(v.Claims) > 0 && allocated:
		v.AllocationAPI, v.Attribution = npuv1alpha1.AllocationAPIDRA, AttributionDevice
	case len(v.Claims) > 0:
		v.AllocationAPI, v.Attribution = npuv1alpha1.AllocationAPIDRA, AttributionPending
		v.DeviceUIDs = nil
	case len(v.Requests) > 0:
		v.AllocationAPI, v.Attribution = npuv1alpha1.AllocationAPIDevicePlugin, AttributionNode
		v.DeviceUIDs = nil
	default:
		return ConsumerView{}, false
	}
	sort.Strings(v.DeviceUIDs)
	return v, true
}

// deviceClassOf 는 claim 의 첫 요청에서 DeviceClass 이름을 꺼낸다. 요청이
// 여럿이면 첫 것만 쓴다 — 화면 라벨 용도이고 정확한 목록은 claim 을 직접 본다.
func deviceClassOf(c *resourcev1.ResourceClaim) string {
	for _, req := range c.Spec.Devices.Requests {
		if req.Exactly != nil && req.Exactly.DeviceClassName != "" {
			return req.Exactly.DeviceClassName
		}
	}
	return ""
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
	var claims resourcev1.ResourceClaimList
	// ResourceClaim 은 클러스터에 없을 수 있다(K8s 1.28 라인). 그때는 DRA 축을
	// 비운 채 device-plugin 축만 낸다 — 목록 전체를 죽이지 않는다.
	if err := s.Reader.List(r.Context(), &claims); err != nil {
		s.Log.V(1).Info("resourceclaims 조회 실패 — DRA 축 없이 진행", "error", err.Error())
	}

	out := make([]ConsumerView, 0, len(pods.Items))
	for i := range pods.Items {
		if v, ok := toConsumerView(pods.Items[i], claims.Items); ok {
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
