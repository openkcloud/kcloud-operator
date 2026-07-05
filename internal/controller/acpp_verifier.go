// ============================================================
// acpp_verifier.go: ACPP 하드웨어 검증 구현 — Node allocatable + 1-파티션 테스트 Pod (spec 전제2/3)
// 상세: VerifyAllocatable 은 Node.status.allocatable 을 읽어 순수 비교(allocatableMet)로 판정한다.
//
//	VerifyAllocation 은 operator 네임스페이스에 1-파티션 리소스를 요청하는 테스트 Pod 를
//	대상 노드에 스케줄해 Running 을 기다린 뒤 정리한다(spec 전제3). kubelet 이 없는 envtest 로는
//	Running 전이를 재현할 수 없어 라이브 실증은 Task 19 로 미룬다(ponytail: 순수 비교 함수만 unit 테스트).
//
// 생성일: 2026-07-23
// ============================================================
package controller

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"kcloud-operator/internal/naming"
	"kcloud-operator/internal/partition"
)

// verifyPollInterval/verifyTimeout 은 테스트 Pod Running 대기 폴링 파라미터(MVP-1 고정값).
const (
	verifyPollInterval = 2 * time.Second
	verifyTimeout      = 180 * time.Second
)

// defaultProbeImage 는 ACPP_PROBE_IMAGE 미설정 시 fallback — air-gap(Harbor mirror) 클러스터는
// 이 이미지가 미러링되어 있지 않으면 ImagePullBackOff 로 verify 가 실패한다(finding #3).
const defaultProbeImage = "registry.k8s.io/pause:3.9"

// liveVerifier 는 실 클러스터(Node/Pod) 를 대상으로 partition.Verifier 를 구현한다.
type liveVerifier struct {
	client     client.Client
	probeImage string
}

// NewLiveVerifier 는 실 클러스터 검증기를 만든다. 검증 Pod 이미지는 ACPP_PROBE_IMAGE 환경변수로
// override 가능(air-gap 에서 미러 경로 지정) — 미설정 시 registry.k8s.io/pause:3.9.
func NewLiveVerifier(c client.Client) partition.Verifier {
	img := os.Getenv("ACPP_PROBE_IMAGE")
	if img == "" {
		img = defaultProbeImage
	}
	return &liveVerifier{client: c, probeImage: img}
}

// allocatableMet 는 node.status.allocatable[resourceName] 이 want 이상인지 판정하는 순수 함수다
// (VerifyAllocatable 의 테스트 가능한 핵심 — 라이브 client 없이 unit 테스트).
func allocatableMet(node *corev1.Node, resourceName string, want int32) bool {
	q, ok := node.Status.Allocatable[corev1.ResourceName(resourceName)]
	if !ok {
		return false
	}
	return q.Value() >= int64(want)
}

// VerifyAllocatable 는 대상 Node 의 allocatable 이 expected 를 모두 충족하는지 확인한다.
// VerifyAllocatable 은 기대 allocatable 로 수렴할 때까지 폴링한다(verifyTimeout). device-plugin 재시작 후
// MIG 조각 재광고에는 수 초~수십 초가 걸리므로 단발 확인은 조기 false 를 내어 불필요한 rollback 을 유발한다.
func (v *liveVerifier) VerifyAllocatable(t partition.Target, expected map[string]int32) (*partition.VerifyResult, error) {
	ctx, cancel := context.WithTimeout(t.Ctx, verifyTimeout)
	defer cancel()
	var lastSnapshot map[string]int32
	for {
		var node corev1.Node
		if err := v.client.Get(ctx, types.NamespacedName{Name: t.NodeName}, &node); err != nil {
			return nil, fmt.Errorf("acpp verify: get node %q: %w", t.NodeName, err)
		}
		snapshot := make(map[string]int32, len(expected))
		converged := true
		for res, want := range expected {
			if !allocatableMet(&node, res, want) {
				converged = false
			}
			if q, ok := node.Status.Allocatable[corev1.ResourceName(res)]; ok {
				snapshot[res] = int32(q.Value())
			}
		}
		lastSnapshot = snapshot
		if converged {
			return &partition.VerifyResult{AllocatableConverged: true, Snapshot: snapshot}, nil
		}
		select {
		case <-ctx.Done():
			return &partition.VerifyResult{AllocatableConverged: false, Snapshot: lastSnapshot}, nil
		case <-time.After(verifyPollInterval):
		}
	}
}

// VerifyAllocation 은 대상 노드에 1-파티션(resourceName: 1) 테스트 Pod 를 스케줄하고
// Running 전이를 기다린 뒤 삭제한다(spec 전제3). 스케줄 실패/타임아웃은 실패로 취급한다.
func (v *liveVerifier) VerifyAllocation(t partition.Target, resourceName string) (*partition.VerifyResult, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "acpp-verify-",
			Namespace:    naming.OperatorNamespace(),
			Labels:       map[string]string{"npu.ai/acpp-verify-pod": "true"},
		},
		Spec: corev1.PodSpec{
			NodeName:      t.NodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "verify",
				Image: v.probeImage,
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceName(resourceName): resource.MustParse("1"),
					},
				},
			}},
		},
	}
	if err := v.client.Create(t.Ctx, pod); err != nil {
		return nil, fmt.Errorf("acpp verify: create test pod: %w", err)
	}
	defer func() {
		_ = v.client.Delete(context.Background(), pod)
	}()

	ctx, cancel := context.WithTimeout(t.Ctx, verifyTimeout)
	defer cancel()
	for {
		var got corev1.Pod
		if err := v.client.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &got); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("acpp verify: get test pod: %w", err)
			}
		} else {
			switch got.Status.Phase {
			case corev1.PodRunning:
				return &partition.VerifyResult{TestPodAllocated: true}, nil
			case corev1.PodFailed:
				return &partition.VerifyResult{TestPodAllocated: false}, nil
			}
		}
		select {
		case <-ctx.Done():
			return &partition.VerifyResult{TestPodAllocated: false}, nil
		case <-time.After(verifyPollInterval):
		}
	}
}
