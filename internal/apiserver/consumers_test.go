// ============================================================
// consumers_test.go: 소비자 귀속 판정 시험
// 상세: 픽스처를 실제 corev1 타입으로 만든다 — 프로덕션이 만들 수 없는 조합으로
// 시험하면 그 시험은 무효다. 이 라인에는 DRA 축이 없으므로 귀속은 node 하나다.
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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fakePodReader 는 Pod 목록만 돌려주는 client.Reader 다.
type fakePodReader struct{ pods *corev1.PodList }

func (f fakePodReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return fmt.Errorf("미구현")
}

func (f fakePodReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	v, ok := list.(*corev1.PodList)
	if !ok {
		return fmt.Errorf("예상 못한 목록 타입 %T", list)
	}
	*v = *f.pods
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

	got, ok := toConsumerView(pod)
	if !ok {
		t.Fatal("가속기 요청 Pod 가 걸러졌다")
	}
	if got.AllocationAPI != "devicePlugin" {
		t.Errorf("AllocationAPI = %q, 기대 devicePlugin", got.AllocationAPI)
	}
	if got.Attribution != AttributionNode {
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
	if _, ok := toConsumerView(pod); ok {
		t.Error("가속기를 요청하지 않은 Pod 가 목록에 들어갔다")
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
	s := &Server{Reader: fakePodReader{pods: consumerFixture()}, Log: logr.Discard()}

	items := getConsumers(t, s, "?node=rngd-1")
	if len(items) != 1 || items[0].Name != "b" {
		t.Fatalf("items = %+v, 기대 rngd-1 의 b 하나", items)
	}
}

func TestHandleConsumersExcludesNonAccelerator(t *testing.T) {
	s := &Server{Reader: fakePodReader{pods: consumerFixture()}, Log: logr.Discard()}

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

func TestHandleConsumersFiltersByVendor(t *testing.T) {
	s := &Server{Reader: fakePodReader{pods: consumerFixture()}, Log: logr.Discard()}

	items := getConsumers(t, s, "?vendor=furiosa")
	if len(items) != 1 || items[0].Name != "b" {
		t.Fatalf("items = %+v, 기대 furiosa 를 쓰는 b 하나", items)
	}
}
