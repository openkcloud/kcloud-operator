// ============================================================
// acpp_rollback_visibility_test.go: 파티션 되돌리기 실패의 상태 노출 envtest
// 상세: 되돌리기가 실패하면 그 사실이 phase 와 조건에 남아야 조정자가 수동 복구 종점을 열 수 있다.
// 생성일: 2026-08-01
// ============================================================
package controller

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/partition/nvidia"
)

// rollbackFailingExec 는 되돌리기 명령만 실패시키는 실행기다.
// 되돌리기는 disable 단계(`-dgi`/`-dci`)를 실행하므로 그 인자로 갈라낸다 — 적용은 성공하고
// 되돌리기만 실패하는, 프로덕션에서 실제로 일어나는 조합을 만든다.
type rollbackFailingExec struct{ rollbacks *int }

func (e rollbackFailingExec) Run(_ context.Context, _, _, _ string, steps []nvidia.CommandStep) error {
	for _, s := range steps {
		for _, a := range s.Argv {
			if a == "-dgi" || a == "-dci" {
				*e.rollbacks++
				return errors.New("simulated rollback failure")
			}
		}
	}
	return nil
}

// rollbackOnlyFailingExec 는 opID 로 apply 와 rollback 호출을 구분해, **되돌리기 호출만** 실패시킨다.
// BuildApplySteps 자체가 사전정리용 -dci/-dgi(Optional) 를 포함하므로(geometry.go), 인자만 보는
// rollbackFailingExec 는 apply 내부에서 이미 실패해(rollbacks 카운터가 apply 단계에서부터 오른다)
// "검증 실패로 되돌리기에 들어간" 경로가 아니라 "적용 자체가 실패한" 경로를 태운다 — 실측으로
// 확인함(OperationID 가 만드는 opID 에 "-apply-"/"-rollback-" 이 박혀 있어 opID 로만 구분 가능하다).
// 검증 실패 이후의 되돌리기(라인 552)를 별도로 타지 않으면 그 분기는 실은 아무도 검증하지 못한다.
type rollbackOnlyFailingExec struct{ rollbacks *int }

func (e rollbackOnlyFailingExec) Run(_ context.Context, opID, _, _ string, steps []nvidia.CommandStep) error {
	if !strings.Contains(opID, "-rollback-") {
		return nil
	}
	*e.rollbacks++
	return errors.New("simulated rollback failure")
}

var _ = Describe("partition rollback visibility", func() {
	// 증명: 적용 자체가 실패해 되돌리기에 들어갔는데 되돌리기까지 실패하면, 그 사실이 phase 와
	//       조건에 남는다. 이 노출이 없으면 조정자가 보상 실패를 영영 못 보고 수동 복구 종점이
	//       열리지 않는다.
	//
	// 이 스펙은 "검증 실패로 되돌리기에 들어간" 경로가 아니다 — BuildApplySteps 자체가 사전정리용
	// -dci/-dgi(Optional) 를 포함하므로(geometry.go), 인자만 보는 rollbackFailingExec 는 검증
	// 이전에 적용 자체를 실패시킨다(실측: opID 에 "-apply-" 가 박혀 있음을 확인함). 그래서
	// r.Verifier = failVerifier{} 는 이 스펙에서 도달하지 않는 설정이다 — 적용 실패 경로(524줄)
	// 를 태우는 스펙으로 고쳐 부른다. 검증 실패 경로(552줄)는 아래 별도 스펙이 다룬다.
	// 깨는 뮤테이션: `_ = backend.Rollback(...)` 로 되돌리면(오류를 다시 버리면) phase 가
	//                RollingBack 에 머물러 실패한다.
	It("surfaces a failed rollback after an apply failure in the phase and condition", func() {
		wipeACPPs()
		DeferCleanup(wipeACPPs)
		labels := map[string]string{"kcloud.ai/rb-visible": "true"}
		seedNvidiaNode("rb-visible-node", labels, true, a30Device("", "Enabled", "Enabled", ""))
		DeferCleanup(func() { cleanupNvidia("rb-visible-node") })

		acpp := mkNvidiaACPP("rb-visible-acpp", labels)
		Expect(k8sClient.Create(ctx, acpp)).To(Succeed())

		rollbacks := 0
		r := nvidiaReconciler()
		r.Verifier = failVerifier{} // 검증 실패 → 되돌리기 진입
		r.NvidiaExecFactory = func(client.Client) nvidia.Executor {
			return rollbackFailingExec{rollbacks: &rollbacks}
		}
		for i := 0; i < 8; i++ {
			_, _ = r.Reconcile(ctx, reconcileReq("rb-visible-acpp"))
		}
		Expect(rollbacks).To(BeNumerically(">", 0), "되돌리기 경로에 도달하지 못했다 — 이 스펙은 아무것도 검증하지 못한다")

		var got npuv1alpha1.AcceleratorPartitionPolicy
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-visible-acpp"}, &got)).To(Succeed())
		Expect(got.Status.Targets).NotTo(BeEmpty())
		Expect(got.Status.Targets[0].Phase).To(Equal(npuv1alpha1.ACPPPhaseRollbackFailed))
		cond := apimeta.FindStatusCondition(got.Status.Targets[0].Conditions, npuv1alpha1.ACPPCondRolledBack)
		Expect(cond).NotTo(BeNil(), "되돌리기 실패인데 조건이 없다")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).NotTo(BeEmpty())
	})

	// 증명: 되돌리기가 성공하면 성공으로 표시된다(성공·실패가 같은 상태로 보이면 안 된다).
	// 이 스펙은 fakeNvidiaExec(항상 성공)를 쓰므로 적용은 통과하고, 검증(failVerifier)만 실패해
	// 검증 실패 되돌리기 경로(552줄)를 태운다 — 위 스펙과 달리 진짜로 검증 실패발 되돌리기다.
	// 깨는 뮤테이션: 성공 경로의 조건 기록을 지우면 조정자가 성공을 진행 중으로 오인해
	//                보상 상한까지 매달린다.
	It("marks a successful rollback distinctly after a verify failure", func() {
		wipeACPPs()
		DeferCleanup(wipeACPPs)
		labels := map[string]string{"kcloud.ai/rb-ok": "true"}
		seedNvidiaNode("rb-ok-node", labels, true, a30Device("", "Enabled", "Enabled", ""))
		DeferCleanup(func() { cleanupNvidia("rb-ok-node") })

		acpp := mkNvidiaACPP("rb-ok-acpp", labels)
		Expect(k8sClient.Create(ctx, acpp)).To(Succeed())
		r := nvidiaReconciler()
		r.Verifier = failVerifier{} // 검증만 실패 — 되돌리기 자체는 성공한다
		for i := 0; i < 8; i++ {
			_, _ = r.Reconcile(ctx, reconcileReq("rb-ok-acpp"))
		}
		var got npuv1alpha1.AcceleratorPartitionPolicy
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-ok-acpp"}, &got)).To(Succeed())
		Expect(got.Status.Targets).NotTo(BeEmpty())
		Expect(got.Status.Targets[0].Phase).NotTo(Equal(npuv1alpha1.ACPPPhaseRollbackFailed))
		cond := apimeta.FindStatusCondition(got.Status.Targets[0].Conditions, npuv1alpha1.ACPPCondRolledBack)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	// 증명: 검증 실패로 되돌리기에 들어갔는데(적용 자체는 성공) 되돌리기까지 실패하면, 그 사실이
	// phase 와 조건에 남고 **여러 pass 를 거쳐도 유지된다** — 위 두 스펙이 각각 "적용 실패+되돌리기
	// 실패" 와 "검증 실패+되돌리기 성공" 을 덮으므로, 남는 조합("검증 실패+되돌리기 실패", 552줄)은
	// 이 스펙이 채운다. 이 조합이 바로 되돌리기 표면화가 존재하는 이유인 주 시나리오다.
	//
	// 관측 seam(a30Device 의 geometry)이 목표와 일치해야 한다 — 되돌리기가 실패하면 GI 가 그대로
	// 남으므로 실제 관측도 계속 목표와 같은 geometry 를 본다. 빈 문자열을 쓰면 diff.Changed 가
	// 항상 참이 되어 routeNvidiaNoDiff 의 no-diff 갈래를 아예 안 타므로, 되돌리기 실패 신호가
	// 두 번째 pass 부터 ExistingMigConfiguration 으로 덮이는 결함을 가린다(실측함).
	// 깨는 뮤테이션: 552줄의 `_ = backend.Rollback(...)` 로 되돌리면 phase 가 RollingBack 에
	//                머물러 실패한다. routeNvidiaNoDiff 의 MigPhaseRolledBack 갈래를 지우면 두
	//                번째 pass 부터 phase 가 Failed(ExistingMigConfiguration)로 덮여 실패한다.
	It("keeps surfacing a failed rollback after a verify failure across multiple passes", func() {
		wipeACPPs()
		DeferCleanup(wipeACPPs)
		labels := map[string]string{"kcloud.ai/rb-verify-fail": "true"}
		seedNvidiaNode("rb-verify-fail-node", labels, true, a30Device("", "Enabled", "Enabled", ""))
		DeferCleanup(func() { cleanupNvidia("rb-verify-fail-node") })

		acpp := mkNvidiaACPP("rb-verify-fail-acpp", labels)
		Expect(k8sClient.Create(ctx, acpp)).To(Succeed())

		rollbacks := 0
		r := nvidiaReconciler()
		r.Verifier = failVerifier{} // 적용은 통과, 검증만 실패 → 검증 실패 되돌리기 진입
		r.NvidiaExecFactory = func(client.Client) nvidia.Executor {
			return rollbackOnlyFailingExec{rollbacks: &rollbacks}
		}
		for i := 0; i < 8; i++ {
			_, _ = r.Reconcile(ctx, reconcileReq("rb-verify-fail-acpp"))
		}
		Expect(rollbacks).To(BeNumerically(">", 0), "되돌리기 경로에 도달하지 못했다 — 이 스펙은 아무것도 검증하지 못한다")

		var mid npuv1alpha1.AcceleratorPartitionPolicy
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-verify-fail-acpp"}, &mid)).To(Succeed())
		Expect(mid.Status.Targets).NotTo(BeEmpty())
		Expect(mid.Status.Targets[0].Phase).To(Equal(npuv1alpha1.ACPPPhaseRollbackFailed), "첫 되돌리기 실패 상태에 도달하지 못했다")

		// 되돌리기 명령이 실제로 실패했으니 GI 는 그대로 남는다 — 다음 관측도 여전히 목표와 같은
		// geometry 를 본다. 빈 문자열을 그대로 두면 diff.Changed 가 항상 참이 되어
		// routeNvidiaNoDiff 의 no-diff 갈래를 아예 안 타므로, 이 프로덕션 상태(되돌리기 실패 후
		// GI 존속)를 여기서 직접 만든다.
		var ndr npuv1alpha1.NodeDeviceReport
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-verify-fail-node"}, &ndr)).To(Succeed())
		ndr.Status.Devices[0].MigCurrentGeometry = nvidia.GeometrySummary("1g.6gb", 4)
		Expect(k8sClient.Status().Update(ctx, &ndr)).To(Succeed())

		for i := 0; i < 4; i++ {
			_, _ = r.Reconcile(ctx, reconcileReq("rb-verify-fail-acpp"))
		}

		var got npuv1alpha1.AcceleratorPartitionPolicy
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-verify-fail-acpp"}, &got)).To(Succeed())
		Expect(got.Status.Targets).NotTo(BeEmpty())
		Expect(got.Status.Targets[0].Phase).To(Equal(npuv1alpha1.ACPPPhaseRollbackFailed),
			"GI 가 그대로인 채 여러 pass 를 거친 뒤 되돌리기 실패 신호가 사라졌다")
		cond := apimeta.FindStatusCondition(got.Status.Targets[0].Conditions, npuv1alpha1.ACPPCondRolledBack)
		Expect(cond).NotTo(BeNil(), "되돌리기 실패인데 조건이 없다")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).NotTo(BeEmpty())
	})

	// 증명: 크래시 복구 경로(resumeNvidiaRecovery, 869줄 부근)도 같은 방식으로 되돌리기 실패를
	// 노출하고, **여러 pass 를 거쳐도** 그 신호가 남는다. 이 함수는 rb 가 없어(하드웨어 mutation 은
	// 이미 끝났다고 간주) 위 두 경로와 시그니처가 다르므로 별도로 확인해야 한다. "이미 이 ACPP 가
	// 소유하고 geometry 도 일치하는데 Verifying 단계에서 크래시했다" 는 저널을 심어
	// owned && isApplyInFlightPhase 진입 조건을 만든다(Task 6/9 의 managed no-diff 픽스처와
	// 같은 패턴) — 관측 seam(a30Device 의 geometry)이 되돌리기 실패 후에도 GI 가 그대로 남은
	// 프로덕션 상태를 그대로 반영한다(빈 문자열이면 diff.Changed 가 항상 참이 되어 이 결함을
	// 가린다 — 실측함).
	//
	// 첫 pass 이후, MigPhase 가 RolledBack 으로 바뀌어 isApplyInFlightPhase 를 벗어난다.
	// routeNvidiaNoDiff 가 그 상태를 handled=true 로 가로채면(ExistingMigConfiguration) 표면화한
	// 신호가 사라진다 — 그 가로채기를 막는 것이 이번 수정이다.
	// 깨는 뮤테이션: routeNvidiaNoDiff 의 MigPhaseRolledBack 갈래를 지우면 두 번째 pass 부터
	//                phase 가 Failed(ExistingMigConfiguration)로 덮여 실패한다.
	It("keeps surfacing a failed crash-recovery rollback across multiple passes", func() {
		wipeACPPs()
		DeferCleanup(wipeACPPs)
		labels := map[string]string{"kcloud.ai/rb-recovery": "true"}
		geom := nvidia.GeometrySummary("1g.6gb", 4)
		seedNvidiaNode("rb-recovery-node", labels, true, a30Device(geom, "Enabled", "Enabled", ""))
		DeferCleanup(func() { cleanupNvidia("rb-recovery-node") })

		acpp := mkNvidiaACPP("rb-recovery-acpp", labels)
		Expect(k8sClient.Create(ctx, acpp)).To(Succeed())
		acpp.Status.ApplyRecords = []npuv1alpha1.ApplyRecord{{
			NodeName: "rb-recovery-node", GPUPCIs: []string{"0000:41:00.0"}, OwnerUID: string(acpp.UID),
			BaselineGPUCount: 1, ExpectedMigCount: 4, ExpectedFullGPUCount: 1,
			Profile: "1g.6gb", Count: 4, MigPhase: npuv1alpha1.MigPhaseVerifying, Generation: acpp.Generation,
		}}
		Expect(k8sClient.Status().Update(ctx, acpp)).To(Succeed())

		rollbacks := 0
		r := nvidiaReconciler()
		r.Verifier = failVerifier{}
		r.NvidiaExecFactory = func(client.Client) nvidia.Executor {
			return rollbackOnlyFailingExec{rollbacks: &rollbacks}
		}
		for i := 0; i < 4; i++ {
			_, _ = r.Reconcile(ctx, reconcileReq("rb-recovery-acpp"))
		}
		Expect(rollbacks).To(BeNumerically(">", 0), "크래시 복구 되돌리기 경로에 도달하지 못했다 — 이 스펙은 아무것도 검증하지 못한다")

		var got npuv1alpha1.AcceleratorPartitionPolicy
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-recovery-acpp"}, &got)).To(Succeed())
		Expect(got.Status.Targets).NotTo(BeEmpty())
		Expect(got.Status.Targets[0].Phase).To(Equal(npuv1alpha1.ACPPPhaseRollbackFailed),
			"여러 pass 후 되돌리기 실패 신호가 사라졌다")
		cond := apimeta.FindStatusCondition(got.Status.Targets[0].Conditions, npuv1alpha1.ACPPCondRolledBack)
		Expect(cond).NotTo(BeNil(), "크래시 복구 되돌리기 실패인데 조건이 없다")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	})
})
