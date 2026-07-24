// ============================================================
// npuclusterpolicy_advertise.go: 장치 광고 주체 선택 스위치(advertiseBy) 처리
// 상세: spec 에서 dra 로 선언된 벤더의 대상 노드에 dra-owned 라벨을 붙이고, 선언을 되돌리면
//       회수한다. 그 라벨이 있는 노드에서는 벤더 device-plugin DaemonSet 이 물러난다
//       (제외는 각 DaemonSet 렌더링에서 affinity 로 건다).
// 생성일: 2026-08-06
// ============================================================

package controller

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/equality"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/intent"
	"kcloud-operator/internal/partition"
)

// 광고 주체 축을 갖는 벤더 중 이 패키지에 아직 상수가 없던 둘. Rebellions ATOM 은 대상이 아니다.
const (
	vendorRngd        = "rngd"
	vendorTenstorrent = "tenstorrent"
)

// advertiseVendors 는 광고 주체 축을 갖는 벤더와 그 선언이다.
func advertiseVendors(p *npuv1alpha1.NPUClusterPolicy) map[string]npuv1alpha1.AdvertiseSpec {
	return map[string]npuv1alpha1.AdvertiseSpec{
		vendorNvidia:      p.Spec.Nvidia.AdvertiseSpec,
		vendorFuriosa:     p.Spec.Furiosa.AdvertiseSpec,
		vendorRngd:        p.Spec.Furiosa.Rngd.AdvertiseSpec,
		vendorTenstorrent: p.Spec.Tenstorrent.AdvertiseSpec,
	}
}

// vendorPresentLabel 은 그 벤더 장치가 이 노드에 있다는 자립 라벨이다(node-manager 부여).
func vendorPresentLabel(vendor string) string {
	return "kcloud.ai/" + vendor + ".present"
}

// reconcileDRAOwnedLabels 는 spec 과 노드 라벨을 맞춘다. 부여와 회수가 같은 함수에 있어야
// 고아 라벨이 남지 않는다 — 붙이기만 하는 코드는 되돌릴 길을 만들지 않는다.
func (r *NPUClusterPolicyReconciler) reconcileDRAOwnedLabels(
	ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	logger := logf.FromContext(ctx)

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return fmt.Errorf("노드 목록 조회 실패: %w", err)
	}

	// DRA 발행 실측. resource.k8s.io 가 없는 클러스터에서는 빈 값이 되고, 그러면 dra 전환은
	// 전부 거절된다 — device-plugin 을 끄기 전에 넘겨받을 쪽이 실재하는지부터 본다.
	dra, err := intent.LoadDRACapability(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("DRA 발행 상태 조회 실패: %w", err)
	}

	var switches []npuv1alpha1.AdvertiseSwitchStatus

	for vendor, adv := range advertiseVendors(policy) {
		key := npuv1alpha1.DRAOwnedNodeLabel(vendor)

		// devicePlugin(기본)이면 대상 노드가 없다 → 남아 있는 라벨은 전부 회수된다.
		selector := labels.Nothing()
		if adv.AdvertisedBy() == npuv1alpha1.AdvertiseByDRA {
			// nil selector 는 "안 좁힌다" 는 뜻이다. LabelSelectorAsSelector(nil) 은
			// Nothing 을 돌려주므로(=아무 노드도 안 맞음) 여기서 Everything 으로 갈라 준다.
			selector = labels.Everything()
			if adv.AdvertiseByNodeSelector != nil {
				s, err := metav1.LabelSelectorAsSelector(adv.AdvertiseByNodeSelector)
				if err != nil {
					return fmt.Errorf("%s advertiseByNodeSelector 해석 실패: %w", vendor, err)
				}
				selector = s
			}
		}

		for i := range nodes.Items {
			node := &nodes.Items[i]
			want := node.Labels[vendorPresentLabel(vendor)] == labelValueTrue &&
				selector.Matches(labels.Set(node.Labels))
			_, has := node.Labels[key]

			// 새로 넘기려는 노드는 두 가지를 먼저 통과해야 한다. 이미 넘어간 노드는 다시 묻지
			// 않는다 — 되돌리는 길(want=false)은 어떤 경우에도 막지 않는다.
			if want && !has {
				refusal, perr := r.refuseAdvertiseSwitch(ctx, vendor, node.Name, dra)
				if perr != nil {
					return perr
				}
				if refusal != nil {
					refusal.Vendor, refusal.Node = vendor, node.Name
					switches = append(switches, *refusal)
					r.Recorder.Eventf(policy, corev1.EventTypeWarning, "AdvertiseSwitchRefused",
						"노드 %s 의 %s 광고 주체 전환 거절: %s", node.Name, vendor, refusal.Reason)
					continue
				}
			}
			if want {
				switches = append(switches, npuv1alpha1.AdvertiseSwitchStatus{
					Vendor: vendor, Node: node.Name, Applied: true,
				})
			}
			if want == has {
				continue
			}

			patch := client.MergeFrom(node.DeepCopy())
			if want {
				if node.Labels == nil {
					node.Labels = map[string]string{}
				}
				node.Labels[key] = labelValueTrue
			} else {
				delete(node.Labels, key)
			}
			if err := r.Patch(ctx, node, patch); err != nil {
				return fmt.Errorf("노드 %s 라벨 %s 갱신 실패: %w", node.Name, key, err)
			}
			logger.Info("DRA 소유 라벨 갱신", "node", node.Name, "label", key, "applied", want)
		}
	}

	return r.recordAdvertiseSwitch(ctx, policy, switches)
}

// refuseAdvertiseSwitch 는 이 노드를 지금 DRA 소유로 넘겨도 되는지 본다. 넘기면 안 되는
// 이유가 있으면 그 사유를, 없으면 nil 을 돌려준다.
func (r *NPUClusterPolicyReconciler) refuseAdvertiseSwitch(
	ctx context.Context, vendor, node string, dra intent.DRACapability,
) (*npuv1alpha1.AdvertiseSwitchStatus, error) {
	// ① 넘겨받을 쪽이 실재하는가. 없으면 device-plugin 을 끈 순간 그 노드의 광고가 양쪽 다 0 이 된다.
	if len(dra.SlicesByNodeDriver[node]) == 0 {
		return &npuv1alpha1.AdvertiseSwitchStatus{
			Reason: "이 노드에 ResourceSlice 를 발행하는 DRA 드라이버가 없다 — 드라이버를 먼저 설치하라",
		}, nil
	}

	// ② 지금 장치를 쥐고 있는 워크로드가 있는가. 판정은 Quiescer 를 그대로 쓴다 —
	// 점유 규칙을 여기에 다시 쓰면 두 규칙이 갈라진다.
	pods, err := advertiseQuiescer(r.Client, vendor).DeviceHoldingPods(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("노드 %s 점유 조회 실패: %w", node, err)
	}
	if len(pods) > 0 {
		return &npuv1alpha1.AdvertiseSwitchStatus{
			Reason:       "장치를 쥔 워크로드가 있다 — 노드를 비우고 다시 시도하라",
			BlockingPods: pods,
		}, nil
	}
	return nil, nil
}

// advertiseQuiescer 는 벤더별 점유 판정기다. 매핑에 없는 벤더는 DRA claim 축만 보는
// 기본 판정기를 쓴다(holdsDevice 는 resourceClaims 를 벤더 무관으로 잡는다).
func advertiseQuiescer(c client.Client, vendor string) *partition.Quiescer {
	switch vendor {
	case vendorNvidia:
		return partition.NvidiaQuiescer(c)
	case vendorRngd:
		return partition.RngdQuiescer(c)
	default:
		return &partition.Quiescer{Client: c}
	}
}

// recordAdvertiseSwitch 는 노드별 전환 판정을 status 에 남긴다. 내용이 그대로면 쓰지 않는다 —
// 매 reconcile 마다 status 를 갱신하면 watch 가 자기 자신을 깨운다.
func (r *NPUClusterPolicyReconciler) recordAdvertiseSwitch(
	ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy, switches []npuv1alpha1.AdvertiseSwitchStatus,
) error {
	sort.Slice(switches, func(i, j int) bool {
		if switches[i].Vendor != switches[j].Vendor {
			return switches[i].Vendor < switches[j].Vendor
		}
		return switches[i].Node < switches[j].Node
	})
	if equality.Semantic.DeepEqual(policy.Status.AdvertiseSwitch, switches) {
		return nil
	}
	patch := client.MergeFrom(policy.DeepCopy())
	policy.Status.AdvertiseSwitch = switches
	if err := r.Status().Patch(ctx, policy, patch); err != nil {
		return fmt.Errorf("광고 주체 전환 상태 기록 실패: %w", err)
	}
	return nil
}

// excludeDRAOwnedNodes 는 이 벤더의 dra-owned 노드를 DaemonSet 스케줄 후보에서 뺀다.
// 라벨이 붙은 노드가 없으면 아무 영향이 없으므로 조건 없이 건다 — 분기를 두면 spec 을
// 되돌린 직후 한 박자 동안 두 광고가 겹칠 수 있다.
func excludeDRAOwnedNodes(spec *corev1.PodSpec, vendor string) {
	appendNodeAffinityRequirements(spec, corev1.NodeSelectorRequirement{
		Key:      npuv1alpha1.DRAOwnedNodeLabel(vendor),
		Operator: corev1.NodeSelectorOpDoesNotExist,
	})
}
