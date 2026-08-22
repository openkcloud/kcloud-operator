// ============================================================
// acceleratorpartitionpolicy_test.go: ACPPValidator 시험
// 상세: 정상/형식오류/노드부재/빈요청 거부 + fail-open(제품 미확인 통과) 못박기.
// 생성일: 2026-08-24 | 수정일: 2026-08-24
// ============================================================
package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

const hostnameLabel = "kubernetes.io/hostname"

func acppNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{hostnameLabel: name}}}
}

func acppNDR(nodeName, model string) *npuv1alpha1.NodeDeviceReport {
	return &npuv1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Spec:       npuv1alpha1.NodeDeviceReportSpec{NodeName: nodeName},
		Status:     npuv1alpha1.NodeDeviceReportStatus{Devices: []npuv1alpha1.DeviceEntry{{Vendor: "nvidia", Model: model}}},
	}
}

func specMIG(node, profile string) npuv1alpha1.AcceleratorPartitionPolicySpec {
	return npuv1alpha1.AcceleratorPartitionPolicySpec{
		NodeSelector: map[string]string{hostnameLabel: node},
		Vendor:       "nvidia",
		Layout:       []npuv1alpha1.PartitionLayout{{Profile: profile, CountPerDevice: 4}},
	}
}

func specEmpty(node string) npuv1alpha1.AcceleratorPartitionPolicySpec {
	return npuv1alpha1.AcceleratorPartitionPolicySpec{
		NodeSelector: map[string]string{hostnameLabel: node},
		Vendor:       "nvidia",
	}
}

func specSharing(node string) npuv1alpha1.AcceleratorPartitionPolicySpec {
	return npuv1alpha1.AcceleratorPartitionPolicySpec{
		NodeSelector: map[string]string{hostnameLabel: node},
		Vendor:       "nvidia",
		Sharing:      &npuv1alpha1.SharingSpec{Mode: npuv1alpha1.SharingModeTimeSliced},
	}
}

// TestACPPValidator: worker1 에 a30, master 에(제품 미확인 표시용) geforce-gtx-970 이 있는
// NodeDeviceReport 를 fake client 에 넣어 정상/거부/fail-open 경로를 함께 확인한다.
//
// brief 의 예시는 "nodeSelector 가 아무 노드도 안 고름" 케이스의 wantMsg 를 한국어 "노드"로
// 들었으나, 이 구현의 거부 메시지는 저장소의 다른 validator 들과 같이 영어로 통일했다
// (spec.nodeSelector ... matches no node) — wantMsg 를 "node" 로 맞춰 실제 메시지와 일치시켰다.
func TestACPPValidator(t *testing.T) {
	cases := []struct {
		name    string
		spec    npuv1alpha1.AcceleratorPartitionPolicySpec
		wantErr bool
		wantMsg string
	}{
		{"정상 MIG", specMIG("k8s-worker1", "1g.6gb"), false, ""},
		{"a30 에 없는 프로파일", specMIG("k8s-worker1", "1g.5gb"), true, "a30"},
		{"형식 오류", specMIG("k8s-worker1", "1g5gb"), true, "profile"},
		{"레이아웃도 sharing 도 없음", specEmpty("k8s-worker1"), true, "layout"},
		{"nodeSelector 가 아무 노드도 안 고름", specMIG("k8s-worker9", "1g.6gb"), true, "node"},
		{"모르는 제품은 통과", specMIG("k8s-master", "1g.5gb"), false, ""},
		{"sharing 만 있는 정책은 통과", specSharing("k8s-worker1"), false, ""},
	}

	c := build(t,
		acppNode("k8s-worker1"), acppNDR("k8s-worker1", "a30"),
		acppNode("k8s-master"), acppNDR("k8s-master", "geforce-gtx-970"),
	)

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acpp := &npuv1alpha1.AcceleratorPartitionPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("p%d", i)},
				Spec:       tc.spec,
			}
			err := validateACPP(context.Background(), c, acpp)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
			if err != nil && tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestACPPValidatorWrongType(t *testing.T) {
	c := build(t)
	if err := validateACPP(context.Background(), c, &npuv1alpha1.NPUClusterPolicy{}); err == nil {
		t.Fatal("wrong type accepted")
	}
}

// 입구가 클러스터 상태에 의존해 정상 요청을 막으면 안 된다 — NodeList 조회 자체가 실패해도
// (API 서버 일시 오류 등) 판정을 보류하고 통과시켜야 한다. 이 시험이 없으면 :67 의
// `return nil` 을 `return err` 로 뒤집어도 아무것도 깨지지 않는다.
func TestACPPValidatorNodeListFailurePasses(t *testing.T) {
	injected := errors.New("injected list failure")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(acppNode("k8s-worker1"), acppNDR("k8s-worker1", "a30")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.NodeList); ok {
					return injected
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	acpp := &npuv1alpha1.AcceleratorPartitionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "list-fail"},
		Spec:       specMIG("k8s-worker1", "1g.6gb"),
	}
	if err := validateACPP(context.Background(), c, acpp); err != nil {
		t.Fatalf("NodeList 조회 실패가 정상 요청을 거절했다: %v", err)
	}
}

// 같은 이유로 NodeDeviceReport 조회 실패도 그 노드에 대한 판정만 건너뛰어야 한다 — :80 의
// `continue` 를 거절로 뒤집어도 이 시험이 없으면 잡히지 않는다.
func TestACPPValidatorNDRGetFailurePasses(t *testing.T) {
	injected := errors.New("injected get failure")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(acppNode("k8s-worker1"), acppNDR("k8s-worker1", "a30")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*npuv1alpha1.NodeDeviceReport); ok {
					return injected
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	acpp := &npuv1alpha1.AcceleratorPartitionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "get-fail"},
		Spec:       specMIG("k8s-worker1", "1g.6gb"),
	}
	if err := validateACPP(context.Background(), c, acpp); err != nil {
		t.Fatalf("NodeDeviceReport 조회 실패가 정상 요청을 거절했다: %v", err)
	}
}
