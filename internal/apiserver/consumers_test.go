// ============================================================
// consumers_test.go: 소비자 귀속 판정 시험
// 상세: 픽스처를 실제 corev1/resourcev1 타입으로 만든다 — 프로덕션이 만들 수
//
//	없는 조합으로 시험하면 그 시험은 무효다.
//
// 생성일: 2026-08-07
// ============================================================
package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fakeReader 는 List 만 채운 client.Reader 다. 핸들러가 Pod 와 ResourceClaim
// 두 목록을 읽으므로 둘 다 돌려준다.
type fakeReader struct {
	pods      *corev1.PodList
	claims    *resourcev1.ResourceClaimList
	claimsErr error
}

func (f fakeReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return fmt.Errorf("미구현")
}

func (f fakeReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch v := list.(type) {
	case *corev1.PodList:
		*v = *f.pods
	case *resourcev1.ResourceClaimList:
		if f.claimsErr != nil {
			return f.claimsErr
		}
		*v = *f.claims
	default:
		return fmt.Errorf("예상 못한 목록 타입 %T", list)
	}
	return nil
}

func podWithLimits(ns, name, node string, limits map[string]string) corev1.Pod {
	rl := corev1.ResourceList{}
	for k, v := range limits {
		rl[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Limits: rl}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestDevicePluginPodIsNodeAttributed(t *testing.T) {
	pod := podWithLimits("infer", "gpu-0", "k8s-worker1", map[string]string{"nvidia.com/gpu": "2"})

	got, ok := toConsumerView(pod, nil)
	if !ok {
		t.Fatal("가속기 요청 Pod 가 걸러졌다")
	}
	if got.AllocationAPI != "devicePlugin" {
		t.Errorf("AllocationAPI = %q, 기대 devicePlugin", got.AllocationAPI)
	}
	if got.Attribution != "node" {
		t.Errorf("Attribution = %q, 기대 node", got.Attribution)
	}
	if len(got.DeviceUIDs) != 0 {
		t.Errorf("DeviceUIDs = %v, 기대 빈 값 — 어느 카드인지 API 로 알 수 없다", got.DeviceUIDs)
	}
	if len(got.Requests) != 1 || got.Requests[0].Quantity != 2 || got.Requests[0].Vendor != "nvidia" {
		t.Errorf("Requests = %+v", got.Requests)
	}
}

func TestNonAcceleratorPodIsFilteredOut(t *testing.T) {
	pod := podWithLimits("default", "nginx", "k8s-worker1", map[string]string{"cpu": "1"})
	if _, ok := toConsumerView(pod, nil); ok {
		t.Error("가속기를 요청하지 않은 Pod 가 목록에 들어갔다")
	}
}

func TestAllocatedDRAClaimIsDeviceAttributed(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infer", Name: "vllm-0"},
		Spec: corev1.PodSpec{
			NodeName:       "rngd-1",
			ResourceClaims: []corev1.PodResourceClaim{{Name: "npu", ResourceClaimName: ptr.To("vllm-0-npu")}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	claim := resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infer", Name: "vllm-0-npu"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{{Name: "r0",
					Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "rngd.furiosa.ai"}}},
			},
		},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{{Device: "npu0", Driver: "rngd.furiosa.ai", Pool: "rngd-1"}},
				},
			},
		},
	}

	got, ok := toConsumerView(pod, []resourcev1.ResourceClaim{claim})
	if !ok {
		t.Fatal("DRA Pod 가 걸러졌다")
	}
	if got.AllocationAPI != "dra" {
		t.Errorf("AllocationAPI = %q, 기대 dra", got.AllocationAPI)
	}
	if got.Attribution != "device" {
		t.Errorf("Attribution = %q, 기대 device", got.Attribution)
	}
	if len(got.DeviceUIDs) != 1 || got.DeviceUIDs[0] != "npu0" {
		t.Errorf("DeviceUIDs = %v, 기대 [npu0]", got.DeviceUIDs)
	}
}

// 미할당 claim 은 node 가 아니라 pending 이다. 둘의 뜻이 다르다 —
// node 는 영영 알 수 없고 pending 은 곧 알게 된다.
func TestUnallocatedDRAClaimIsPending(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infer", Name: "vllm-1"},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: "npu", ResourceClaimName: ptr.To("vllm-1-npu")}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	claim := resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infer", Name: "vllm-1-npu"},
	}

	got, ok := toConsumerView(pod, []resourcev1.ResourceClaim{claim})
	if !ok {
		t.Fatal("미할당 DRA Pod 가 걸러졌다")
	}
	if got.Attribution != "pending" {
		t.Errorf("Attribution = %q, 기대 pending", got.Attribution)
	}
	if len(got.DeviceUIDs) != 0 {
		t.Errorf("DeviceUIDs = %v, 기대 빈 값", got.DeviceUIDs)
	}
}

func consumerFixture() *corev1.PodList {
	return &corev1.PodList{Items: []corev1.Pod{
		podWithLimits("infer", "a", "k8s-worker1", map[string]string{"nvidia.com/gpu": "1"}),
		podWithLimits("infer", "b", "rngd-1", map[string]string{"furiosa.ai/rngd": "2"}),
		podWithLimits("default", "nginx", "k8s-worker1", map[string]string{"cpu": "1"}),
	}}
}

func getConsumers(t *testing.T, s *Server, query string) []ConsumerView {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleConsumers(rec, httptest.NewRequest("GET", "/api/v1/consumers"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []ConsumerView `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("응답 파싱 실패: %v — body=%s", err, rec.Body.String())
	}
	return page.Items
}

func TestHandleConsumersFiltersByNode(t *testing.T) {
	s := &Server{
		Reader: fakeReader{pods: consumerFixture(), claims: &resourcev1.ResourceClaimList{}},
		Log:    logr.Discard(),
	}

	items := getConsumers(t, s, "?node=rngd-1")
	if len(items) != 1 || items[0].Name != "b" {
		t.Fatalf("items = %+v, 기대 rngd-1 의 b 하나", items)
	}
}

// 비가속기 Pod 는 필터 없이도 목록에 들어가지 않는다.
func TestHandleConsumersExcludesNonAccelerator(t *testing.T) {
	s := &Server{
		Reader: fakeReader{pods: consumerFixture(), claims: &resourcev1.ResourceClaimList{}},
		Log:    logr.Discard(),
	}

	items := getConsumers(t, s, "")
	if len(items) != 2 {
		t.Fatalf("items = %+v, 기대 2건(nginx 제외)", items)
	}
	for _, v := range items {
		if v.Name == "nginx" {
			t.Error("비가속기 Pod 가 목록에 들어갔다")
		}
	}
}

// 1.28 라인처럼 resourceclaims CRD 가 없으면 조회가 실패한다. 그때 목록 전체를
// 죽이지 말고 device-plugin 축만 내야 한다.
func TestHandleConsumersSurvivesMissingResourceClaims(t *testing.T) {
	s := &Server{
		Reader: fakeReader{pods: consumerFixture(), claimsErr: fmt.Errorf("no matches for kind ResourceClaim")},
		Log:    logr.Discard(),
	}

	items := getConsumers(t, s, "?vendor=furiosa")
	if len(items) != 1 || items[0].Name != "b" {
		t.Fatalf("items = %+v, 기대 furiosa 를 쓰는 b 하나", items)
	}
	if items[0].Attribution != AttributionNode {
		t.Errorf("Attribution = %q, 기대 node", items[0].Attribution)
	}
}
