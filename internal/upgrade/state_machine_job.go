// ============================================================
// state_machine_job.go: WP-C-1 driver Job 모드 상태 핸들러 (Mode=job)
// 상세: state_machine.go 의 handleUpgrading/handleValidating/handleRollback 이
//       Mode=job 일 때 early-return 으로 진입하는 대체 경로. 검증된 안전 순서
//       (cordon→drain→install→validate→uncordon) 와 validator 체인은 그대로
//       재사용하되, "DS 이미지 patch + pod 삭제" 를 "install Job 생성/재생성" 으로,
//       "DS pod Ready 관찰" 을 "Job 성공 종료 + NDR 일치" 로 재정의한다.
//       daemonset 경로 본문은 바이트 불변(additive-only).
//       install Job 의 신원은 이미지 + DRIVER_VERSION 두 축이다(jobTargetsInstall).
// 생성일: 2026-07-16 | 수정일: 2026-08-07
// ============================================================

package upgrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/driverjob"
	"kcloud-operator/internal/metrics"
	"kcloud-operator/internal/naming"
)

// isJobMode 는 정책이 install-Job 모드(Mode=job)인지 반환한다.
// 빈 값/"daemonset" 은 기존 DS 경로(false).
func isJobMode(policy *v1alpha1.DriverInstallPolicy) bool {
	return policy != nil && policy.Spec.Driver.Mode == "job"
}

// driverNotLoaded 는 노드의 NDR 이 vendor/model 매칭 device 를 "존재하지만 driverLoaded=false"
// 로 보고하는지 반환한다(Job 모드 self-heal 트리거용, handleIdle). Job 모드는 상주 DS 가 없어
// detector 는 driverVersion 을 dpkg 패키지에서 읽으므로, 모듈만 언로드(재부팅/수동 rmmod)되면
// driverLoaded=false 이지만 driverVersion 은 남는다 — 버전 일치만으로는 복구를 트리거하지 못하는
// 갭을 이 헬퍼가 메운다. NDR 미존재 / 매칭 device 없음 / 로드됨 이면 false. 빈 model 정책은
// vendor 만 매칭한다.
func (m *UpgradeStateMachine) driverNotLoaded(ctx context.Context, nodeName, vendor, model string) (bool, error) {
	var ndr v1alpha1.NodeDeviceReport
	if err := m.Get(ctx, types.NamespacedName{Name: nodeName}, &ndr); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, d := range ndr.Status.Devices {
		if !strings.EqualFold(d.Vendor, vendor) {
			continue
		}
		if model != "" && !strings.EqualFold(d.Model, model) {
			continue
		}
		if !d.DriverLoaded {
			return true, nil
		}
	}
	return false, nil
}

// ─────────────────────────────────────────────
// Upgrading (job): install Job 생성/재생성
// ─────────────────────────────────────────────

// handleUpgradingJob 은 desired 이미지로 install Job 을 생성(또는 잔여 Job 재생성)하고
// Validating 으로 전이한다. 결정적 Job 이름이 곧 lease 이므로 Get-before-Create 로
// 중복 생성을 방지한다.
func (m *UpgradeStateMachine) handleUpgradingJob(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	logger := logf.FromContext(ctx)

	desiredImage := policy.Spec.Driver.Image
	desiredVersion := state.Status.DesiredVersion
	if desiredVersion == "" {
		desiredVersion = policy.Spec.Driver.Version
	}

	// Q2: 업그레이드 착수 시점에 rollback 대상(이전 버전 이미지)을 캡처한다. Job 모드는 상주
	// DS 가 없어 이전 이미지를 읽을 소스가 없으므로, DIP image 의 variant 접미사 + PreviousVersion
	// 으로 검증된 build tag 를 재구성해 저장한다. 재구성 불가 시 빈 값 유지 → rollback 은 Failed(안전).
	captureJobPreviousImage(state, desiredImage, policy.Spec.Driver.Installer)

	jobName := naming.InstallJobName(state.Spec.Vendor, state.Spec.Model, state.Spec.NodeName)
	var job batchv1.Job
	err := m.Get(ctx, types.NamespacedName{Name: jobName, Namespace: driverjob.Namespace}, &job)
	if apierrors.IsNotFound(err) {
		desired := driverjob.RenderInstallJob(policy, state.Spec.NodeName, desiredImage, desiredVersion)
		if err := m.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, 0, fmt.Errorf("install Job 생성 실패: %w", err)
		}
		m.Recorder.Eventf(state, corev1.EventTypeNormal, "InstallJobCreated",
			"노드 %s install Job 생성: %s (image=%s, version=%s)",
			state.Spec.NodeName, jobName, desiredImage, desiredVersion)
		return m.transitionTo(state, v1alpha1.UpgradeStateValidating, "install Job 생성", 20*time.Second)
	}
	if err != nil {
		return false, 0, fmt.Errorf("install Job 조회 실패: %w", err)
	}

	// 잔여 Job 존재(이전 사이클 산물). 목표와 불일치면 삭제 후 재생성(requeue → NotFound 경로).
	if !jobTargetsInstall(&job, desiredImage, desiredVersion) {
		logger.Info("이전 사이클 install Job 목표 불일치 — 삭제 후 재생성",
			"job", jobName,
			"currentImage", jobContainerImage(&job), "desiredImage", desiredImage,
			"currentVersion", jobDriverVersion(&job), "desiredVersion", desiredVersion)
		if err := m.deleteInstallJob(ctx, &job); client.IgnoreNotFound(err) != nil {
			return false, 0, fmt.Errorf("잔여 install Job 삭제 실패: %w", err)
		}
		return true, 5 * time.Second, nil
	}

	// 동일 이미지 Job 이 이미 존재(요청 중복/재진입) — Validating 으로 진행.
	return m.transitionTo(state, v1alpha1.UpgradeStateValidating, "install Job 재사용", 20*time.Second)
}

// ─────────────────────────────────────────────
// Validating (job): Job 성공 종료 + NDR 일치 확인
// ─────────────────────────────────────────────

// handleValidatingJob 은 install Job 의 성공 종료를 확인한 뒤, daemonset 과 동일한
// validator 체인(DriverModule=NDR + DevicePlugin)을 재사용해 최종 검증한다.
func (m *UpgradeStateMachine) handleValidatingJob(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	logger := logf.FromContext(ctx)

	// detector 차단 라벨 제거(daemonset 과 동일 — detector 재spawn→NDR 갱신 가능케).
	if err := m.EnsureUpgradingBlockingLabelRemoved(ctx, state.Spec.NodeName); err != nil {
		logger.Error(err, "Validating(job) 진입 시 blocking 라벨 제거 실패", "node", state.Spec.NodeName)
	}

	// ─── 1. Job 상태 판정 ───
	jobName := naming.InstallJobName(state.Spec.Vendor, state.Spec.Model, state.Spec.NodeName)
	var job batchv1.Job
	err := m.Get(ctx, types.NamespacedName{Name: jobName, Namespace: driverjob.Namespace}, &job)
	jobFound := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return false, 0, fmt.Errorf("install Job 조회 실패: %w", err)
	}

	// hard failure: backoffLimit 소진(Failed condition) → CrashLoop 과 동일 취급.
	// activeDeadlineSeconds 초과도 여기로 들어온다(DeadlineExceeded).
	if jobFound && jobFailed(&job) {
		if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.RollbackOnFailure {
			return m.transitionTo(state, v1alpha1.UpgradeStateRollback, "install Job 실패: 롤백 시작", 0)
		}
		return m.jobTransitionToFailed(ctx, state, "install Job 실패(backoffLimit 소진): 수동 조치 필요")
	}

	// install Job 이 아직 돌고 있으면 검증 예산을 쓰지 않는다. Job 에는 자기 시한
	// (activeDeadlineSeconds, 기본 30m)이 있고 초과하면 위 실패 분기가 받는다. 여기서 별도
	// 시계를 돌리면 정상 진행 중인 설치를 실패로 단정한다 — 라이브 실측: apt 로 313MB 를 받는
	// 도중 검증 예산 10분이 끝나 롤백으로 밀려났고, 그 롤백이 없는 이미지를 집어 노드가 굳었다.
	if jobFound && !jobComplete(&job) {
		logger.Info("install Job 진행 중 — 완료 대기", "job", jobName)
		return true, 10 * time.Second, nil
	}

	// ─── 2. 검증 예산 가드 — Job 이 끝난 뒤(또는 사라진 뒤)부터 잰다 ───
	validationTimeout := parseDuration("", 15*time.Minute)
	if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.ValidationTimeout != "" {
		validationTimeout = parseDuration(policy.Spec.UpgradePolicy.ValidationTimeout, 15*time.Minute)
	}
	since := state.Status.LastTransitionTime.Time
	if jobFound && job.Status.CompletionTime != nil && job.Status.CompletionTime.After(since) {
		since = job.Status.CompletionTime.Time
	}
	if !since.IsZero() && time.Since(since) > validationTimeout {
		if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.RollbackOnFailure {
			return m.transitionTo(state, v1alpha1.UpgradeStateRollback, "검증 타임아웃(job): 롤백 시작", 0)
		}
		return m.jobTransitionToFailed(ctx, state, "검증 타임아웃(job): 수동 조치 필요")
	}

	if !jobFound {
		// Job 미발견(아직 미생성 / 완료 후 TTL GC). 검증 예산 안에서 재확인 대기.
		logger.Info("install Job 미발견 — 재확인 대기", "job", jobName)
		return true, 10 * time.Second, nil
	}

	// device-plugin 재스캔 레이스 방지: drain 으로 재생성된 device-plugin Pod 가
	// install Job 완료(모듈 재로드→/dev 재생성) '전'에 기동했다면, 기동 1회 스캔에서
	// 디바이스를 못 봐 0개를 광고한 채 굳는다(allocatable=0). Job 완료 시각 이전에 뜬
	// plugin 을 1회 바운스해 디바이스 존재 상태에서 재스캔시킨다(자기제한적: 재기동 Pod 는
	// 완료 이후 시각이라 재삭제되지 않음).
	if job.Status.CompletionTime != nil {
		restarted, rerr := m.restartStaleDevicePluginPods(ctx, state.Spec.NodeName, *job.Status.CompletionTime)
		if rerr != nil {
			return false, 0, fmt.Errorf("stale device-plugin 재시작 실패: %w", rerr)
		}
		if restarted > 0 {
			return true, 10 * time.Second, nil // 재스캔 Pod Ready 대기 후 validator 진입
		}
	}

	// ─── 3. Job Complete → validator 체인 순차 실행(완전 재사용) ───
	desiredVersion := state.Status.DesiredVersion
	for _, v := range defaultValidators {
		m.Recorder.Eventf(state, corev1.EventTypeNormal,
			fmt.Sprintf("UpgradeValidator-%s-Started", v.Name()),
			"validator 실행(job): %s (node=%s)", v.Name(), state.Spec.NodeName)

		res, err := v.Run(ctx, m.Client, state.Spec.NodeName, state.Spec.Vendor, desiredVersion)
		if err != nil {
			return false, 0, fmt.Errorf("validator %s 실행 실패: %w", v.Name(), err)
		}
		if !res.Passed {
			m.Recorder.Eventf(state, corev1.EventTypeNormal,
				fmt.Sprintf("UpgradeValidator-%s-Failed", v.Name()),
				"validator %s 미통과(재시도 대기): %s", v.Name(), res.Message)
			return true, 10 * time.Second, nil
		}
		m.Recorder.Eventf(state, corev1.EventTypeNormal,
			fmt.Sprintf("UpgradeValidator-%s-Passed", v.Name()),
			"validator %s 통과: %s", v.Name(), res.Message)
	}

	// ─── 4. 모든 validator 통과 → 검증 성공 ───
	state.Status.CurrentVersion = state.Status.DesiredVersion
	m.Recorder.Eventf(state, corev1.EventTypeNormal, "UpgradeValidated",
		"노드 %s 드라이버 검증 성공(job): %s", state.Spec.NodeName, state.Status.DesiredVersion)
	return m.transitionTo(state, v1alpha1.UpgradeStateUncordoning, "검증 성공(job)", 0)
}

// restartStaleDevicePluginPods 는 since(=install Job 완료 시각) 이전에 기동한 노드의
// device-plugin Pod 를 삭제해 재스캔을 유도한다. 반환값은 삭제 건수.
// device-plugin 은 벤더별로 다른 네임스페이스(kube-system / kcloud)에 있으므로 전 네임스페이스를
// 조회하고 nodeName + device-plugin 이름/라벨로 필터한다(deleteDevicePluginPods 와 동일 규칙).
func (m *UpgradeStateMachine) restartStaleDevicePluginPods(ctx context.Context, nodeName string, since metav1.Time) (int, error) {
	var podList corev1.PodList
	if err := m.List(ctx, &podList); err != nil {
		return 0, err
	}
	logger := logf.FromContext(ctx)
	deleted := 0
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != nodeName || pod.DeletionTimestamp != nil {
			continue
		}
		isDP := false
		for key, val := range pod.Labels {
			if key == "app.kubernetes.io/name" && strings.Contains(val, "device-plugin") {
				isDP = true
				break
			}
		}
		if !isDP && strings.Contains(pod.Name, "device-plugin") {
			isDP = true
		}
		if !isDP {
			continue
		}
		// Job 완료 이후(=디바이스 존재 후) 기동한 Pod 는 최신 스캔 — 유지.
		if pod.Status.StartTime == nil || !pod.Status.StartTime.Before(&since) {
			continue
		}
		logger.Info("stale device-plugin Pod 재시작(재스캔)", "pod", pod.Name, "node", nodeName)
		if err := m.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// ─────────────────────────────────────────────
// Rollback (job): 이전 버전 install Job 재실행
// ─────────────────────────────────────────────

// handleRollbackJob 은 이전 버전 이미지로 install Job 을 재실행한다. 1차 범위는 "재-Job 실행"
// 으로 한정하며 DS 이미지 복원 시맨틱은 없다. 이전 이미지(캡처본 또는 variant 재구성) 부재 시
// Failed(안전)로 전이한다.
//
// 카운팅 규칙: RollbackAttempts 는 rollback Job 을 실제로 새로 생성하는 시점(아래 "absent →
// create")에만 1회 증가한다. 잔여 Job 삭제→requeue 단계는 카운트하지 않아 이중 증가를 막는다.
func (m *UpgradeStateMachine) handleRollbackJob(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	logger := logf.FromContext(ctx)

	maxRollbacks := int32(3)
	if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.MaxRollbackAttempts > 0 {
		maxRollbacks = policy.Spec.UpgradePolicy.MaxRollbackAttempts
	}

	prevVersion := state.Status.PreviousVersion
	if prevVersion == "" {
		metrics.RecordUpgradeComplete(state.Spec.Vendor, "failure")
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "RollbackFailed",
			"롤백할 이전 버전 없음(job): 수동 조치 필요 (node=%s)", state.Spec.NodeName)
		return m.jobTransitionToFailed(ctx, state, "이전 버전 없음(job): 수동 조치 필요")
	}

	// rollback 대상 이미지: 캡처된 PreviousImage 우선, 없으면 DIP variant 재구성.
	prevImage := state.Status.PreviousImage
	if prevImage == "" {
		prevImage = reconstructPrevImage(policy.Spec.Driver.Image, policy.Spec.Driver.Installer, prevVersion)
	}
	if prevImage == "" {
		metrics.RecordUpgradeComplete(state.Spec.Vendor, "failure")
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "RollbackRefused",
			"이전 이미지 미보유 + variant 재구성 실패(job): 수동 조치 필요 (node=%s)", state.Spec.NodeName)
		return m.jobTransitionToFailed(ctx, state, "rollback 대상 이미지 재구성 실패(job): 수동 조치 필요")
	}

	jobName := naming.InstallJobName(state.Spec.Vendor, state.Spec.Model, state.Spec.NodeName)
	var job batchv1.Job
	getErr := m.Get(ctx, types.NamespacedName{Name: jobName, Namespace: driverjob.Namespace}, &job)
	jobExists := getErr == nil
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return false, 0, fmt.Errorf("rollback: install Job 조회 실패: %w", getErr)
	}

	if jobExists && jobTargetsInstall(&job, prevImage, prevVersion) {
		// 이미 rollback 대상 Job 이 존재.
		if jobFailed(&job) {
			// rollback Job 도 실패 → 삭제해 다음 사이클에서 재시도(재생성 시 카운트).
			logger.Info("rollback Job 실패 — 삭제 후 재시도", "job", jobName)
			if err := m.deleteInstallJob(ctx, &job); client.IgnoreNotFound(err) != nil {
				return false, 0, fmt.Errorf("실패한 rollback Job 삭제 실패: %w", err)
			}
			return true, 5 * time.Second, nil
		}
		// 진행 중/완료 → Validating 이 관찰. (카운트 없음)
		state.Status.DesiredVersion = prevVersion
		return m.transitionTo(state, v1alpha1.UpgradeStateValidating,
			fmt.Sprintf("rollback Job 진행(job): %s", prevVersion), 20*time.Second)
	}

	if jobExists {
		// 대상과 다른(실패한 desired 이미지) 잔여 Job → 삭제 후 재생성. (카운트 없음)
		logger.Info("rollback: 잔여 install Job 삭제 후 재생성",
			"job", jobName, "current", jobContainerImage(&job), "rollbackTo", prevImage)
		if err := m.deleteInstallJob(ctx, &job); client.IgnoreNotFound(err) != nil {
			return false, 0, fmt.Errorf("rollback: 잔여 install Job 삭제 실패: %w", err)
		}
		return true, 5 * time.Second, nil
	}

	// Job 부재 → rollback Job 을 새로 생성 = 1회 attempt.
	state.Status.RollbackAttempts++
	if state.Status.RollbackAttempts > maxRollbacks {
		metrics.RecordUpgradeComplete(state.Spec.Vendor, "failure")
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "RollbackExhausted",
			"롤백 %d회 초과(max=%d): 수동 조치 필요 (node=%s)",
			state.Status.RollbackAttempts-1, maxRollbacks, state.Spec.NodeName)
		return m.jobTransitionToFailed(ctx, state, fmt.Sprintf("롤백 %d회 초과: 수동 조치 필요", maxRollbacks))
	}

	metrics.RecordRollback(state.Spec.Vendor)
	// RenderInstallJob 이 아니라 RenderRollbackJob 이다 — 롤백 Job 은 다운그레이드를 허용받아야
	// 하고 정책 선언 버전이 아니라 지시받은 이전 버전을 설치해야 한다. 일반 설치 Job 으로 렌더링하면
	// installer 자신의 다운그레이드 가드가 거부해 롤백이 무동작이 된다(2026-08-10 라이브).
	desired := driverjob.RenderRollbackJob(policy, state.Spec.NodeName, prevImage, prevVersion)
	if err := m.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, 0, fmt.Errorf("rollback install Job 생성 실패: %w", err)
	}
	state.Status.DesiredVersion = prevVersion
	m.Recorder.Eventf(state, corev1.EventTypeWarning, "RollbackStarted",
		"노드 %s 롤백 시작(job): → %s (image=%s)", state.Spec.NodeName, prevVersion, prevImage)
	metrics.RecordUpgradeComplete(state.Spec.Vendor, "rollback")
	return m.transitionTo(state, v1alpha1.UpgradeStateValidating,
		fmt.Sprintf("롤백 시작(job): %s", prevVersion), 20*time.Second)
}

// ─────────────────────────────────────────────
// job 헬퍼
// ─────────────────────────────────────────────

// jobTransitionToFailed 는 Job 모드 Failed 전이 시 quiesce 복구 + 라벨 정리 후 Failed 로 전이한다
// (daemonset 의 Failed 전이 정리 순서와 동일).
func (m *UpgradeStateMachine) jobTransitionToFailed(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	msg string,
) (bool, time.Duration, error) {
	logger := logf.FromContext(ctx)
	if err := m.RestoreQuiescedDeployments(ctx, state); err != nil {
		logger.Error(err, "Failed 전이 중 quiesce 복구 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
	}
	if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
		logger.Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
			"Failed 전이 중 라벨 제거 실패 (수동 조치 필요): node=%s err=%v", state.Spec.NodeName, err)
	}
	return m.transitionTo(state, v1alpha1.UpgradeStateFailed, msg, 0)
}

// deleteInstallJob 은 install Job 을 background propagation 으로 삭제한다(파드 cascade 삭제).
func (m *UpgradeStateMachine) deleteInstallJob(ctx context.Context, job *batchv1.Job) error {
	return m.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
}

// captureJobPreviousImage 는 Q2 에 따라 rollback 대상 이미지를 DUS status 에 캡처한다.
// 이미 캡처됐거나 PreviousVersion 이 없으면 no-op. 재구성 결과가 검증된 build tag 일 때만 저장한다.
func captureJobPreviousImage(state *v1alpha1.DriverUpgradeState, desiredImage, installer string) {
	if state.Status.PreviousImage != "" || state.Status.PreviousVersion == "" {
		return
	}
	candidate := reconstructPrevImage(desiredImage, installer, state.Status.PreviousVersion)
	if candidate != "" {
		state.Status.PreviousImage = candidate
	}
}

// reconstructPrevImage 는 desired 이미지의 variant 접미사(-vN)와 prevVersion 을 결합해
// 이전 버전 이미지를 재구성한다. variant 미검출/검증 실패 시 빈 문자열.
func reconstructPrevImage(dipImage, installer, prevVersion string) string {
	if dipImage == "" || prevVersion == "" {
		return ""
	}
	// apt/script 인스톨러는 이미지가 버전을 담지 않는다 — 드라이버 버전은 DRIVER_VERSION 환경
	// 변수로 들어간다. 그런데도 태그를 버전으로 갈아 끼우면 레지스트리에 없는 이미지가 만들어지고,
	// rollback Job 이 ImagePullBackOff 로 앉아 노드가 cordon 상태로 고착된다(라이브 실측).
	// 되돌릴 대상은 같은 이미지 + 이전 버전이다.
	if installer != v1alpha1.DriverInstallerNGC {
		return dipImage
	}
	variant := extractImageVariantSuffix(dipImage)
	if variant == "" {
		return ""
	}
	idx := strings.LastIndex(dipImage, ":")
	if idx < 0 {
		return ""
	}
	candidate := dipImage[:idx+1] + prevVersion + variant
	if !isVerifiedBuildTag(candidate) {
		return ""
	}
	return candidate
}

// jobContainerImage 는 Job pod template 의 첫 컨테이너 이미지를 반환한다.
func jobContainerImage(job *batchv1.Job) string {
	cs := job.Spec.Template.Spec.Containers
	if len(cs) > 0 {
		return cs[0].Image
	}
	return ""
}

// jobTargetsInstall 은 잔여 Job 이 지금 하려는 설치(이미지 + 목표 버전)와 같은 것인지 반환한다.
//
// 이미지만으로는 판정할 수 없다. apt/script 인스톨러는 같은 이미지가 모든 드라이버 버전을
// 설치하고 목표 버전은 DRIVER_VERSION env 로만 들어간다 — 그래서 지난 사이클이 남긴 완료된
// Job 과 이번 사이클의 Job 이 이미지 상으로는 구별되지 않는다. 라이브 실측(2026-08-07
// `.91` k8s-worker1): 595.84 사이클 종료 12초 뒤 시작한 580.173.02 사이클이 앞 사이클의
// 완료된 Job 을 재사용해, 580 설치를 한 번도 만들지 않은 채 validator 가 앞 Job 의 결과를
// 10분간 채점했다.
//
// DRIVER_VERSION 이 없는 Job(구 operator 산물)은 불일치로 본다 — 삭제 후 재생성이 안전한 쪽이다.
func jobTargetsInstall(job *batchv1.Job, image, version string) bool {
	return jobContainerImage(job) == image && jobDriverVersion(job) == version
}

// jobDriverVersion 은 Job pod template 첫 컨테이너의 DRIVER_VERSION env 값이다(없으면 "").
func jobDriverVersion(job *batchv1.Job) string {
	cs := job.Spec.Template.Spec.Containers
	if len(cs) == 0 {
		return ""
	}
	for _, e := range cs[0].Env {
		if e.Name == "DRIVER_VERSION" {
			return e.Value
		}
	}
	return ""
}

// jobComplete 는 Job 이 성공 종료했는지(Complete condition 또는 Succeeded>0) 반환한다.
func jobComplete(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return job.Status.Succeeded > 0
}

// jobFailed 는 Job 이 hard failure(Failed condition, backoffLimit 소진 등)인지 반환한다.
func jobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
