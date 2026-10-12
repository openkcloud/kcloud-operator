// ============================================================
// state_machine_job_test.go: Job 모드 상태 핸들러 단위 테스트
// 상세: fake client 기반. handleIdle 초기설치, handleUpgradingJob/handleValidatingJob/
//       handleRollbackJob 전이, daemonset 무영향(isJobMode)을 검증한다.
// 생성일: 2026-07-16 | 수정일: 2026-10-13
// ============================================================

package upgrade

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/driverjob"
	"kcloud-operator/internal/naming"
)

// 공유 테스트 상수(goconst 회피 + 케이스 일관성).
const (
	jNode   = "n1"
	jDUSNm  = "n1-nvidia"
	jVendor = "nvidia"
	jModel  = "generic"
	imgOld  = "reg/nv:580.126.09-v16"
	imgNew  = "reg/nv:590.48.01-v16"
	verOld  = "580.126.09"
	verNew  = "590.48.01"
	val10m  = "10m"
)

// ── 헬퍼 ─────────────────────────────────────────

func makeJobDIP(version, image string, autoUpgrade, rollbackOnFailure bool) *v1alpha1.DriverInstallPolicy {
	return &v1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: jVendor},
		Spec: v1alpha1.DriverInstallPolicySpec{
			Vendor: jVendor,
			Model:  jModel,
			// ngc: 이미지가 드라이버 버전을 담는 방식. 이 파일의 기존 시험들은 버전별 태그
			// 재구성을 전제로 쓰였으므로 그 방식을 명시한다.
			Driver: v1alpha1.DriverSpec{Version: version, Image: image, Mode: "job",
				Installer: v1alpha1.DriverInstallerNGC},
			UpgradePolicy: &v1alpha1.UpgradePolicy{
				AutoUpgrade:       autoUpgrade,
				RollbackOnFailure: rollbackOnFailure,
			},
		},
	}
}

func makeJobDUS(state, current, desired string) *v1alpha1.DriverUpgradeState {
	return &v1alpha1.DriverUpgradeState{
		ObjectMeta: metav1.ObjectMeta{Name: jDUSNm},
		Spec: v1alpha1.DriverUpgradeStateSpec{
			NodeName: jNode, Vendor: jVendor, Model: jModel,
		},
		Status: v1alpha1.DriverUpgradeStateStatus{
			State:              state,
			CurrentVersion:     current,
			DesiredVersion:     desired,
			LastTransitionTime: metav1.Now(),
		},
	}
}

func makeInstallJob(complete, failed bool) *batchv1.Job {
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.InstallJobName(jVendor, jModel, jNode),
			Namespace: driverjob.Namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "driver-install", Image: imgNew}}},
			},
		},
	}
	if complete {
		j.Status.Succeeded = 1
		j.Status.Conditions = append(j.Status.Conditions,
			batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	}
	if failed {
		j.Status.Failed = 6
		j.Status.Conditions = append(j.Status.Conditions,
			batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue})
	}
	return j
}

// ── isJobMode ───────────────────────────────────

func TestIsJobMode(t *testing.T) {
	if isJobMode(nil) {
		t.Error("nil policy 는 job 모드 아님")
	}
	ds := &v1alpha1.DriverInstallPolicy{
		Spec: v1alpha1.DriverInstallPolicySpec{Vendor: jVendor, Driver: v1alpha1.DriverSpec{Version: "1.0"}},
	} // mode 미지정 = daemonset
	if isJobMode(ds) {
		t.Error("mode 미지정(daemonset) 이 job 으로 판정됨")
	}
	if !isJobMode(makeJobDIP("1.0", "img:1.0-v1", true, true)) {
		t.Error("mode=job 이 job 으로 판정 안 됨")
	}
}

// ── handleIdle 초기 설치 ────────────────────

// Job 모드 + currentVersion="" 이면 autoUpgrade 무관하게 초기 설치(UpgradeRequired)로 전이.
func TestHandleIdleJob_InitialInstall_IgnoresAutoUpgrade(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateIdle, "", "")
	// autoUpgrade=false 인데도 초기 설치는 진행되어야 한다.
	dip := makeJobDIP(verOld, imgOld, false, true)
	sm := newUpgradeSMWithRecorder(dus, dip)

	req, _, err := sm.handleIdle(context.Background(), dus, dip)
	if err != nil {
		t.Fatalf("handleIdle 실패: %v", err)
	}
	if !req {
		t.Error("requeue 기대")
	}
	if dus.Status.State != v1alpha1.UpgradeStateRequired {
		t.Errorf("state=%q, want UpgradeRequired (Job 초기 설치)", dus.Status.State)
	}
	if dus.Status.DesiredVersion != verOld {
		t.Errorf("desiredVersion=%q, want %q", dus.Status.DesiredVersion, verOld)
	}
}

// 버전 일치(cur==des)여도 NDR.driverLoaded=false 면 install Job 을 트리거해야 한다
// (self-heal — 재부팅/수동 rmmod 로 모듈 언로드된 job 모드 노드 복구).
func TestHandleIdleJob_DriverNotLoaded_TriggersReinstall(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateIdle, verNew, verNew)
	dip := makeJobDIP(verNew, imgNew, false, true) // autoUpgrade=false 여도 복구 진행
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: jNode},
		Spec:       v1alpha1.NodeDeviceReportSpec{NodeName: jNode},
		Status: v1alpha1.NodeDeviceReportStatus{
			Devices: []v1alpha1.DeviceEntry{
				{Vendor: jVendor, Model: jModel, DriverLoaded: false, DriverVersion: verNew, Count: 1},
			},
		},
	}
	sm := newUpgradeSMWithRecorder(dus, dip, ndr)

	if _, _, err := sm.handleIdle(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleIdle 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateRequired {
		t.Errorf("state=%q, want UpgradeRequired (driverLoaded=false self-heal)", dus.Status.State)
	}
}

// Job 모드 self-heal(driverLoaded=false)도 허용 목록 밖 버전이면 설치하지 않고 UnverifiedVersion 으로 간다.
// 장치가 로드되지 않았으므로 HostAhead 게이트는 관여하지 않는다.
func TestHandleIdleJob_SelfHeal_UnverifiedVersion(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateIdle, verNew, verNew)
	dip := makeJobDIP(verNew, imgNew, false, true)
	dip.Spec.VerifiedVersions = []string{verOld}
	sm := newUpgradeSMWithRecorder(dus, dip, makeNDRWithDriver(false, verNew))

	if _, _, err := sm.handleIdle(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleIdle 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateUnverifiedVersion {
		t.Fatalf("state=%q, want UnverifiedVersion (허용 목록 밖 버전은 self-heal 도 설치하지 않는다)", dus.Status.State)
	}
	if !hasEvent(drainEvents(sm), "UnverifiedVersion") {
		t.Error("UnverifiedVersion 이벤트가 없다")
	}
}

// TrackOnly 정책은 driverLoaded=false self-heal 엣지에서도 install Job 을 트리거하지
// 않아야 한다 — DriverSpec.TrackOnly 계약: 설치 경로 없음.
func TestHandleIdleJob_TrackOnly_NeverTriggersInstall(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateIdle, verNew, verNew)
	dip := makeJobDIP(verNew, imgNew, false, true)
	dip.Spec.Driver.TrackOnly = true
	// driverLoaded=false 여도(self-heal 엣지) TrackOnly 면 재설치 트리거 금지.
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: jNode},
		Spec:       v1alpha1.NodeDeviceReportSpec{NodeName: jNode},
		Status: v1alpha1.NodeDeviceReportStatus{
			Devices: []v1alpha1.DeviceEntry{
				{Vendor: jVendor, Model: jModel, DriverLoaded: false, DriverVersion: verNew, Count: 1},
			},
		},
	}
	sm := newUpgradeSMWithRecorder(dus, dip, ndr)

	if _, _, err := sm.handleIdle(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleIdle 실패: %v", err)
	}
	if dus.Status.State == v1alpha1.UpgradeStateRequired {
		t.Error("TrackOnly 인데 driverLoaded=false self-heal 로 install Job 트리거됨 (계약 위반)")
	}
}

// driverLoaded=true + 버전 일치면 Idle 유지(불필요한 재설치 트리거 금지).
func TestHandleIdleJob_DriverLoaded_StaysIdle(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateIdle, verNew, verNew)
	dip := makeJobDIP(verNew, imgNew, true, true)
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: jNode},
		Spec:       v1alpha1.NodeDeviceReportSpec{NodeName: jNode},
		Status: v1alpha1.NodeDeviceReportStatus{
			Devices: []v1alpha1.DeviceEntry{
				{Vendor: jVendor, Model: jModel, DriverLoaded: true, DriverVersion: verNew, Count: 1},
			},
		},
	}
	sm := newUpgradeSMWithRecorder(dus, dip, ndr)

	if _, _, err := sm.handleIdle(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleIdle 실패: %v", err)
	}
	if dus.Status.State == v1alpha1.UpgradeStateRequired {
		t.Error("driverLoaded=true + 버전 일치인데 재설치 트리거됨 (불필요)")
	}
}

// driverNotLoaded 헬퍼 단위 검증.
func TestDriverNotLoaded(t *testing.T) {
	mkNDR := func(loaded bool) *v1alpha1.NodeDeviceReport {
		return &v1alpha1.NodeDeviceReport{
			ObjectMeta: metav1.ObjectMeta{Name: jNode},
			Spec:       v1alpha1.NodeDeviceReportSpec{NodeName: jNode},
			Status: v1alpha1.NodeDeviceReportStatus{
				Devices: []v1alpha1.DeviceEntry{
					{Vendor: jVendor, Model: jModel, DriverLoaded: loaded, DriverVersion: verNew},
				},
			},
		}
	}
	// 로드 안 됨 → true
	sm := newUpgradeSMWithRecorder(mkNDR(false))
	if nl, err := sm.driverNotLoaded(context.Background(), jNode, jVendor, jModel); err != nil || !nl {
		t.Errorf("driverLoaded=false 인데 notLoaded=%v err=%v", nl, err)
	}
	// 로드됨 → false
	sm2 := newUpgradeSMWithRecorder(mkNDR(true))
	if nl, err := sm2.driverNotLoaded(context.Background(), jNode, jVendor, jModel); err != nil || nl {
		t.Errorf("driverLoaded=true 인데 notLoaded=%v err=%v", nl, err)
	}
	// NDR 부재 → false(에러 아님)
	sm3 := newUpgradeSMWithRecorder()
	if nl, err := sm3.driverNotLoaded(context.Background(), jNode, jVendor, jModel); err != nil || nl {
		t.Errorf("NDR 부재인데 notLoaded=%v err=%v", nl, err)
	}
}

// daemonset 모드에서는 autoUpgrade=false 이면 초기 설치 분기가 발동하지 않아야 한다(회귀 0).
func TestHandleIdleDaemonset_NoInitialInstallBranch(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateIdle, "", "")
	// daemonset(mode 미지정) + autoUpgrade 없음.
	dip := &v1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: jVendor},
		Spec: v1alpha1.DriverInstallPolicySpec{
			Vendor: jVendor, Model: jModel,
			Driver: v1alpha1.DriverSpec{Version: verOld},
		},
	}
	sm := newUpgradeSMWithRecorder(dus, dip)

	if _, _, err := sm.handleIdle(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleIdle 실패: %v", err)
	}
	if dus.Status.State == v1alpha1.UpgradeStateRequired {
		t.Error("daemonset 모드가 Job 초기 설치 분기로 전이됨 (회귀)")
	}
}

// ── handleUpgradingJob ───────────────────────────

func TestHandleUpgradingJob_CreatesJob(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateUpgrading, verOld, verNew)
	// handleIdle 이 Upgrading 진입 전에 PreviousVersion 을 세팅한다(캡처의 소스).
	dus.Status.PreviousVersion = verOld
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip)

	req, _, err := sm.handleUpgradingJob(context.Background(), dus, dip)
	if err != nil {
		t.Fatalf("handleUpgradingJob 실패: %v", err)
	}
	if !req {
		t.Error("requeue 기대")
	}
	if dus.Status.State != v1alpha1.UpgradeStateValidating {
		t.Errorf("state=%q, want Validating", dus.Status.State)
	}
	var job batchv1.Job
	jn := naming.InstallJobName(jVendor, jModel, jNode)
	if err := sm.Get(context.Background(), types.NamespacedName{Name: jn, Namespace: driverjob.Namespace}, &job); err != nil {
		t.Fatalf("install Job 미생성: %v", err)
	}
	if jobContainerImage(&job) != imgNew {
		t.Errorf("job image=%q, want %q", jobContainerImage(&job), imgNew)
	}
	// rollback 대상 이미지 캡처 (PreviousVersion=verOld + variant -v16)
	if dus.Status.PreviousImage != imgOld {
		t.Errorf("PreviousImage 캡처 실패: got %q, want %q", dus.Status.PreviousImage, imgOld)
	}
}

// ── handleValidatingJob ──────────────────────────

func TestHandleValidatingJob_CompleteAndNDRMatch_Uncordoning(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateValidating, verOld, verNew)
	dip := makeJobDIP(verNew, imgNew, true, true)
	job := makeInstallJob(true, false)
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: jNode},
		Spec:       v1alpha1.NodeDeviceReportSpec{NodeName: jNode},
		Status: v1alpha1.NodeDeviceReportStatus{
			Devices: []v1alpha1.DeviceEntry{
				{Vendor: jVendor, Model: jModel, DriverLoaded: true, DriverVersion: verNew, Count: 1},
			},
		},
	}
	dp := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jVendor + "-device-plugin-abc",
			Namespace: "kube-system",
			Labels:    map[string]string{"app.kubernetes.io/name": jVendor + "-device-plugin"},
		},
		Spec: corev1.PodSpec{NodeName: jNode},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}},
		},
	}
	sm := newUpgradeSMWithRecorder(dus, dip, job, ndr, dp)

	if _, _, err := sm.handleValidatingJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleValidatingJob 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateUncordoning {
		t.Errorf("state=%q, want Uncordoning", dus.Status.State)
	}
	if dus.Status.CurrentVersion != verNew {
		t.Errorf("currentVersion=%q, want %q (검증 성공 후 갱신)", dus.Status.CurrentVersion, verNew)
	}
}

func TestHandleValidatingJob_NotComplete_Requeue(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateValidating, verOld, verNew)
	dip := makeJobDIP(verNew, imgNew, true, true)
	job := makeInstallJob(false, false) // 미완료
	sm := newUpgradeSMWithRecorder(dus, dip, job)

	req, after, err := sm.handleValidatingJob(context.Background(), dus, dip)
	if err != nil {
		t.Fatalf("handleValidatingJob 실패: %v", err)
	}
	if !req || after == 0 {
		t.Error("미완료 Job 은 requeue 기대")
	}
	if dus.Status.State != v1alpha1.UpgradeStateValidating {
		t.Errorf("state=%q, want Validating 유지", dus.Status.State)
	}
}

func TestHandleValidatingJob_Failed_RollbackOnFailure(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateValidating, verOld, verNew)
	dip := makeJobDIP(verNew, imgNew, true, true) // rollbackOnFailure=true
	job := makeInstallJob(false, true)            // 실패
	sm := newUpgradeSMWithRecorder(dus, dip, job)

	if _, _, err := sm.handleValidatingJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleValidatingJob 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateRollback {
		t.Errorf("state=%q, want Rollback (Job 실패+RollbackOnFailure)", dus.Status.State)
	}
}

func TestHandleValidatingJob_Failed_NoRollback_Failed(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateValidating, verOld, verNew)
	dip := makeJobDIP(verNew, imgNew, true, false) // rollbackOnFailure=false
	job := makeInstallJob(false, true)
	sm := newUpgradeSMWithRecorder(dus, dip, job)

	if _, _, err := sm.handleValidatingJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleValidatingJob 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateFailed {
		t.Errorf("state=%q, want Failed (Job 실패+rollback 비활성)", dus.Status.State)
	}
}

// ── handleRollbackJob ────────────────────────────

func TestHandleRollbackJob_NoPreviousVersion_Failed(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRollback, verNew, verNew)
	dus.Status.PreviousVersion = "" // 이전 버전 없음
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip)

	if _, _, err := sm.handleRollbackJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRollbackJob 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateFailed {
		t.Errorf("state=%q, want Failed (이전 버전 없음)", dus.Status.State)
	}
}

func TestHandleRollbackJob_CreatesRollbackJob(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRollback, verNew, verNew)
	dus.Status.PreviousVersion = verOld
	dus.Status.PreviousImage = imgOld // 캡처본 존재
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip)

	if _, _, err := sm.handleRollbackJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRollbackJob 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateValidating {
		t.Errorf("state=%q, want Validating (rollback Job 생성 후)", dus.Status.State)
	}
	if dus.Status.DesiredVersion != verOld {
		t.Errorf("desiredVersion=%q, want %q (이전 버전)", dus.Status.DesiredVersion, verOld)
	}
	if dus.Status.RollbackAttempts != 1 {
		t.Errorf("rollbackAttempts=%d, want 1", dus.Status.RollbackAttempts)
	}
	var job batchv1.Job
	jn := naming.InstallJobName(jVendor, jModel, jNode)
	if err := sm.Get(context.Background(), types.NamespacedName{Name: jn, Namespace: driverjob.Namespace}, &job); err != nil {
		t.Fatalf("rollback Job 미생성: %v", err)
	}
	if jobContainerImage(&job) != imgOld {
		t.Errorf("rollback job image=%q, want %q (이전 버전)", jobContainerImage(&job), imgOld)
	}

	// 정책은 allowDowngrade 를 켜지 않았다(makeJobDIP 는 이 필드를 건드리지 않는다). 그래도
	// 롤백 Job 은 다운그레이드를 허용받아야 한다 — 아니면 installer 자신의 가드가 설치를
	// 거부해 롤백이 무동작이 된다.
	//
	// **이 단언이 호출부를 지킨다.** 렌더러 쪽 시험(driverjob/rollbackjob_test.go)만으로는
	// 이 자리를 RenderInstallJob 으로 되돌려도 아무도 실패하지 않는다.
	var allowDowngrade string
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "ALLOW_DOWNGRADE" {
			allowDowngrade = e.Value
		}
	}
	if allowDowngrade != "true" {
		t.Errorf("rollback job ALLOW_DOWNGRADE=%q, want \"true\" — 롤백이 자기 가드에 막힌다", allowDowngrade)
	}
}

// ── install Job 신원(이미지+목표 버전) ────────────
// apt/script 인스톨러는 같은 이미지가 모든 드라이버 버전을 설치한다 — 목표 버전은 이미지 태그가
// 아니라 DRIVER_VERSION env 로 들어간다. 그래서 이미지만으로 Job 을 식별하면 지난 사이클이 남긴
// 완료된 Job 을 이번 사이클의 Job 으로 착각해, 새 설치 없이 앞 Job 의 결과로 검증이 끝나고
// 검증 예산 만료→불필요한 rollback→불필요한 재부팅으로 이어진다.

// makeAptJobDIP 는 apt 인스톨러 정책이다 — 모든 버전이 같은 이미지를 쓴다.
func makeAptJobDIP(version, image string) *v1alpha1.DriverInstallPolicy {
	dip := makeJobDIP(version, image, true, true)
	dip.Spec.Driver.Installer = v1alpha1.DriverInstallerAPT
	return dip
}

// makeVersionedInstallJob 는 DRIVER_VERSION env 를 담은 install Job 이다(RenderInstallJob 과 동일 형태).
func makeVersionedInstallJob(image, version string, complete bool) *batchv1.Job {
	j := makeInstallJob(complete, false)
	j.Spec.Template.Spec.Containers[0].Image = image
	j.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "RUN_MODE", Value: "job"},
		{Name: "DRIVER_VERSION", Value: version},
	}
	return j
}

// 같은 이미지 + 다른 목표 버전인 잔여 Job 은 이번 사이클의 Job 이 아니다 — 삭제 후 재생성.
func TestHandleUpgradingJob_StaleJobSameImageOtherVersion_Recreated(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateUpgrading, verNew, verOld)
	dip := makeAptJobDIP(verOld, imgNew)
	stale := makeVersionedInstallJob(imgNew, verNew, true) // 지난 사이클(verNew) 산물, 완료됨
	sm := newUpgradeSMWithRecorder(dus, dip, stale)

	if _, _, err := sm.handleUpgradingJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleUpgradingJob 실패: %v", err)
	}
	if dus.Status.State == v1alpha1.UpgradeStateValidating {
		t.Error("지난 사이클의 완료된 Job 을 이번 사이클 Job 으로 재사용했다 — 목표 버전이 설치되지 않는다")
	}
	var job batchv1.Job
	jn := naming.InstallJobName(jVendor, jModel, jNode)
	if err := sm.Get(context.Background(), types.NamespacedName{Name: jn, Namespace: driverjob.Namespace}, &job); err == nil {
		t.Errorf("잔여 Job 이 삭제되지 않음 (DRIVER_VERSION=%q, 목표=%q)", jobDriverVersion(&job), verOld)
	}
}

// 같은 이미지 + 같은 목표 버전이면 이번 사이클의 Job 이다 — 재사용(재생성 금지).
func TestHandleUpgradingJob_SameImageSameVersion_Reused(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateUpgrading, verOld, verNew)
	dip := makeAptJobDIP(verNew, imgNew)
	mine := makeVersionedInstallJob(imgNew, verNew, false) // 이번 사이클, 진행 중
	sm := newUpgradeSMWithRecorder(dus, dip, mine)

	if _, _, err := sm.handleUpgradingJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleUpgradingJob 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateValidating {
		t.Errorf("state=%q, want Validating (같은 버전 Job 재사용)", dus.Status.State)
	}
	var job batchv1.Job
	jn := naming.InstallJobName(jVendor, jModel, jNode)
	if err := sm.Get(context.Background(), types.NamespacedName{Name: jn, Namespace: driverjob.Namespace}, &job); err != nil {
		t.Error("진행 중인 이번 사이클 Job 이 삭제됨 — 설치가 중단된다")
	}
}

// rollback 도 같은 신원 규칙을 쓴다 — 실패한 desired 버전 Job 을 rollback Job 으로 오인하면
// 되돌리기가 한 번도 실행되지 않는다.
func TestHandleRollbackJob_StaleJobSameImageOtherVersion_Recreated(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRollback, verOld, verNew)
	dus.Status.PreviousVersion = verOld
	dip := makeAptJobDIP(verNew, imgNew)
	failedDesired := makeVersionedInstallJob(imgNew, verNew, true) // 되돌릴 대상이 아닌 Job
	sm := newUpgradeSMWithRecorder(dus, dip, failedDesired)

	if _, _, err := sm.handleRollbackJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRollbackJob 실패: %v", err)
	}
	var job batchv1.Job
	jn := naming.InstallJobName(jVendor, jModel, jNode)
	err := sm.Get(context.Background(), types.NamespacedName{Name: jn, Namespace: driverjob.Namespace}, &job)
	if err == nil && jobDriverVersion(&job) == verNew {
		t.Error("desired 버전 Job 을 rollback Job 으로 오인 — 되돌리기가 실행되지 않는다")
	}
}

// ── job status 헬퍼 ──────────────────────────────

func TestJobStatusHelpers(t *testing.T) {
	if c := makeInstallJob(true, false); !jobComplete(c) || jobFailed(c) {
		t.Error("complete job 판정 오류")
	}
	if f := makeInstallJob(false, true); jobComplete(f) || !jobFailed(f) {
		t.Error("failed job 판정 오류")
	}
	if r := makeInstallJob(false, false); jobComplete(r) || jobFailed(r) {
		t.Error("running job 판정 오류")
	}
}

func TestReconstructPrevImage(t *testing.T) {
	cases := []struct {
		dipImage, installer, prevVersion string
		baked                            bool
		want                             string
	}{
		// ngc: 이미지가 드라이버를 담으므로 버전별 태그를 재구성한다.
		{imgNew, v1alpha1.DriverInstallerNGC, verOld, false, imgOld},
		{"reg/nv:1.7.8-v3", v1alpha1.DriverInstallerNGC, "1.7.7", false, "reg/nv:1.7.7-v3"},
		{"reg/nv:590.48.01", v1alpha1.DriverInstallerNGC, verOld, false, ""}, // variant 없음 → 재구성 불가
		{"", v1alpha1.DriverInstallerNGC, verOld, false, ""},                 // 이미지 없음
		{imgNew, v1alpha1.DriverInstallerNGC, "", false, ""},                 // prevVersion 없음
		// apt/script: 이미지는 버전과 무관하다. 태그를 갈아 끼우면 없는 이미지가 만들어져
		// rollback Job 이 ImagePullBackOff 로 앉는다 — 같은 이미지를 그대로 쓴다.
		{imgNew, v1alpha1.DriverInstallerAPT, verOld, false, imgNew},
		{"reg/nv:580.159.03-v179", v1alpha1.DriverInstallerAPT, "580.173.02", false, "reg/nv:580.159.03-v179"},
		{imgNew, v1alpha1.DriverInstallerScript, verOld, false, imgNew},
		{imgNew, v1alpha1.DriverInstallerAPT, "", false, ""}, // prevVersion 없음은 여전히 재구성 불가
		// script + imageVersionBaked: 이미지에 버전이 구워져 있어(tenstorrent-driver-ds:2.8.0-v1,
		// entrypoint 가 baked 버전을 권위로 삼는다) 같은 이미지로는 이전 버전을 설치할 수 없다 —
		// ngc 처럼 이전 버전 태그를 재구성한다.
		{"reg/tt:2.11.0-v1", v1alpha1.DriverInstallerScript, "2.8.0", true, "reg/tt:2.8.0-v1"},
		{"reg/tt:2.11.0-v1", v1alpha1.DriverInstallerScript, "2.8.0", false, "reg/tt:2.11.0-v1"},
		{"reg/tt:2.11.0", v1alpha1.DriverInstallerScript, "2.8.0", true, ""}, // variant 없음 → 재구성 불가(Failed 안전)
		{"reg/nv:580.159.03-v179", v1alpha1.DriverInstallerAPT, "580.173.02", true, "reg/nv:580.173.02-v179"},
	}
	for _, c := range cases {
		if got := reconstructPrevImage(c.dipImage, c.installer, c.prevVersion, c.baked); got != c.want {
			t.Errorf("reconstructPrevImage(%q,%q,%q,%v)=%q, want %q",
				c.dipImage, c.installer, c.prevVersion, c.baked, got, c.want)
		}
	}
}

// TestRestartStaleDevicePluginPods 는 install Job 완료 시각 이전에 기동한 device-plugin
// Pod 만 삭제(재스캔 유도)하고, 완료 이후 기동 Pod 와 비-plugin Pod 는 보존함을 고정한다.
// device-plugin 재스캔 레이스(업그레이드 후 allocatable=0) 회귀를 방지한다.
func TestRestartStaleDevicePluginPods(t *testing.T) {
	since := metav1.NewTime(metav1.Now().Time)
	mkPod := func(name string, isDP bool, offset time.Duration) *corev1.Pod {
		labels := map[string]string{}
		if isDP {
			labels["app.kubernetes.io/name"] = "kcloud-tt-device-plugin"
		}
		st := metav1.NewTime(since.Add(offset))
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kcloud", Labels: labels},
			Spec:       corev1.PodSpec{NodeName: jNode},
			Status:     corev1.PodStatus{StartTime: &st},
		}
	}
	stale := mkPod("tt-dp-stale", true, -time.Minute) // 완료 전 기동 → 삭제
	fresh := mkPod("tt-dp-fresh", true, time.Minute)  // 완료 후 기동 → 보존
	other := mkPod("some-app", false, -time.Minute)   // 비-plugin → 보존
	sm := newUpgradeSMWithRecorder(stale, fresh, other)

	n, err := sm.restartStaleDevicePluginPods(context.Background(), jNode, since)
	if err != nil {
		t.Fatalf("restartStaleDevicePluginPods 실패: %v", err)
	}
	if n != 1 {
		t.Fatalf("삭제 건수=%d, want 1 (stale 만)", n)
	}
	var got corev1.Pod
	if err := sm.Get(context.Background(), types.NamespacedName{Name: "tt-dp-stale", Namespace: "kcloud"}, &got); err == nil {
		t.Error("stale device-plugin Pod 가 삭제되지 않음")
	}
	if err := sm.Get(context.Background(), types.NamespacedName{Name: "tt-dp-fresh", Namespace: "kcloud"}, &got); err != nil {
		t.Error("fresh device-plugin Pod 가 삭제됨(보존돼야)")
	}
}

// TestHandleRollbackJob_AptInstallerKeepsImage 는 apt 인스톨러에서 rollback Job 이 정책 이미지를
// 그대로 쓰는 것을 고정한다. 버전으로 태그를 갈아 끼우면 레지스트리에 없는 이미지가 되어
// Job 이 ImagePullBackOff 로 앉고 노드가 cordon 상태로 고착된다.
func TestHandleRollbackJob_AptInstallerKeepsImage(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRollback, verOld, verNew)
	dus.Status.PreviousVersion = verOld
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Spec.Driver.Installer = v1alpha1.DriverInstallerAPT
	sm := newUpgradeSMWithRecorder(dus, dip)

	if _, _, err := sm.handleRollbackJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRollingBackJob 실패: %v", err)
	}
	var job batchv1.Job
	jn := naming.InstallJobName(jVendor, jModel, jNode)
	if err := sm.Get(context.Background(), types.NamespacedName{Name: jn, Namespace: driverjob.Namespace}, &job); err != nil {
		t.Fatalf("rollback Job 미생성: %v", err)
	}
	if got := jobContainerImage(&job); got != imgNew {
		t.Errorf("rollback job image=%q, want %q (정책 이미지 그대로)", got, imgNew)
	}
	if dus.Status.DesiredVersion != verOld {
		t.Errorf("desiredVersion=%q, want %q (이전 버전으로 되돌림)", dus.Status.DesiredVersion, verOld)
	}
}

// TestHandleValidatingJob_RunningJobDoesNotConsumeBudget 는 install Job 이 아직 돌고 있는 동안
// 검증 예산이 만료되어 롤백으로 밀려나지 않는 것을 고정한다. 진행 중인 설치를 예산 만료로
// 중단시키면, 이어진 롤백이 없는 이미지를 집어 노드가 굳는 결과로 이어진다.
func TestHandleValidatingJob_RunningJobDoesNotConsumeBudget(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateValidating, verOld, verNew)
	dus.Status.LastTransitionTime = metav1.NewTime(time.Now().Add(-30 * time.Minute))
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Spec.UpgradePolicy.ValidationTimeout = val10m
	running := makeInstallJob(false, false)
	sm := newUpgradeSMWithRecorder(dus, dip, running)

	requeue, _, err := sm.handleValidatingJob(context.Background(), dus, dip)
	if err != nil {
		t.Fatalf("handleValidatingJob 실패: %v", err)
	}
	if !requeue {
		t.Error("진행 중인 Job 은 재확인 대기여야 한다")
	}
	if dus.Status.State != v1alpha1.UpgradeStateValidating {
		t.Errorf("state=%q, want Validating — 진행 중인 설치가 예산 만료로 중단됐다", dus.Status.State)
	}
}

// TestHandleValidatingJob_BudgetStartsAfterJobCompletion 는 Job 이 끝난 뒤부터 검증 예산이
// 흐르는 것을 고정한다. 완료 직후라면 Validating 진입이 아무리 오래됐어도 타임아웃이 아니다.
func TestHandleValidatingJob_BudgetStartsAfterJobCompletion(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateValidating, verOld, verNew)
	dus.Status.LastTransitionTime = metav1.NewTime(time.Now().Add(-30 * time.Minute))
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Spec.UpgradePolicy.ValidationTimeout = val10m
	done := makeInstallJob(true, false)
	now := metav1.NewTime(time.Now())
	done.Status.CompletionTime = &now
	sm := newUpgradeSMWithRecorder(dus, dip, done)

	if _, _, err := sm.handleValidatingJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleValidatingJob 실패: %v", err)
	}
	if dus.Status.State == v1alpha1.UpgradeStateRollback {
		t.Error("완료 직후인데 검증 타임아웃으로 롤백됐다")
	}
}

// 검증 예산이 끝나 Failed 로 갈 때 메시지에 복구 절차(uncordon·정책 version 정렬·allowDowngrade)
// 가 있어야 한다 — "수동 조치 필요" 만으로는 무엇을 할지 알 수 없다(외부 보고 재현).
func TestHandleValidatingJob_TimeoutFailedMessageHasRecovery(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateValidating, verOld, verNew)
	dus.Status.LastTransitionTime = metav1.NewTime(time.Now().Add(-30 * time.Minute))
	dus.Status.Message = "validator DriverModule 미통과: host kernel module=580.178.04 ≠ desired=" + verNew
	dip := makeJobDIP(verNew, imgNew, true, false)
	dip.Spec.UpgradePolicy.ValidationTimeout = val10m
	done := makeInstallJob(true, false)
	past := metav1.NewTime(time.Now().Add(-20 * time.Minute))
	done.Status.CompletionTime = &past
	sm := newUpgradeSMWithRecorder(dus, dip, done)

	if _, _, err := sm.handleValidatingJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleValidatingJob 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateFailed {
		t.Fatalf("state=%q, want Failed", dus.Status.State)
	}
	for _, want := range []string{"kubectl uncordon " + dus.Spec.NodeName, "spec.driver.version", "allowDowngrade=true", "580.178.04", "목표 " + verNew, "kubectl delete dus " + dus.Name, "DriverUpgradeState 를 재생성"} {
		if !strings.Contains(dus.Status.Message, want) {
			t.Errorf("메시지에 %q 가 없다: %q", want, dus.Status.Message)
		}
	}
}

// detector 가 PCI 식별자로 제품명을 실으면 장치 쪽 model 만 "generic" 에서 "a30" 으로 구체화된다
// — DriverUpgradeState.spec.model 은 생성 시점 값이라 갱신되지 않는다. 두 값이 갈리는 조합을
// 놓치면 재부팅 게이트와 자가복구 게이트가 모든 장치를 걸러 조용히 멈춘다. 양방향을 다 고정한다.
func TestDeviceModelMatchesTreatsGenericAsWildcard(t *testing.T) {
	for _, tc := range []struct {
		name, deviceModel, specModel string
		want                         bool
	}{
		{"둘 다 미판정", "generic", "generic", true},
		{"장치만 판정됨 — 기록은 미판정 시점 값", "a30", "generic", true},
		{"기록만 판정됨 — 이미지 되돌린 뒤", "generic", "a30", true},
		{"둘 다 판정됐고 같다", "a30", "a30", true},
		{"둘 다 판정됐고 다르다", "a2", "a30", false},
		{"spec 이 비면 벤더 전체", "a30", "", true},
		{"대소문자 무시", "A30", "a30", true},
		// 표준 PCI DB 계층이 붙으면 장치 이름만 구체화된다(10de:20f1 → a100-pcie-40gb).
		// 제품군 이름으로 쓴 spec 이 안 걸리면 정책이 nil 이 되어 아무 일도 안 일어난다.
		{"장치가 구체화됨 — spec 은 제품군", "a100-pcie-40gb", "a100", true},
		{"기록이 구체적 — 장치가 제품군", "a100", "a100-pcie-40gb", true},
		{"제품군이 다르다", "a100-pcie-40gb", "a30", false},
		{"접두어 경계 없음 — a1 은 a100 이 아니다", "a100-pcie-40gb", "a1", false},
		{"표기가 통째로 바뀜 — 접두어로 못 덮는다", "a100-sxm4-40gb", "a100-pcie-40gb", false},
	} {
		if got := DeviceModelMatches(tc.deviceModel, tc.specModel); got != tc.want {
			t.Errorf("%s: DeviceModelMatches(%q, %q) = %v, want %v",
				tc.name, tc.deviceModel, tc.specModel, got, tc.want)
		}
	}
}

// ── kernelImages: 커널별 사전 컴파일 이미지 선택 ──────────

// TestExtractKernelImageVersion 은 커널 이미지 태그 앞머리의 드라이버 버전 토큰만 뽑는지 본다.
// variant·커널·빌드 접미사는 버려야 하고, 버전 토큰이 없으면 빈 문자열이어야 한다.
func TestExtractKernelImageVersion(t *testing.T) {
	cases := []struct{ name, image, want string }{
		{"커널 태그", "reg/kcloud/tenstorrent-driver-ds:2.11.0-k5.15.0-190-generic-v1", "2.11.0"},
		{"variant 태그", "reg/r/rebellions-driver-ds:2026.3.1-rngd-k6.8.0-138-generic-v1", "2026.3.1"},
		{"NVIDIA 태그", "reg/nv:580.126.09-k5.15.0-171-generic-v1", "580.126.09"},
		{"하위 번호 포함", "reg/x:1.9.8-3-k5.15.0-190-generic-v1", "1.9.8-3"},
		{"버전 토큰 없음", "reg/x:not-a-verified-tag", ""},
		{"태그 없음", "reg/x/no-colon-here", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractKernelImageVersion(tc.image); got != tc.want {
				t.Fatalf("extractKernelImageVersion(%q) = %q, want %q", tc.image, got, tc.want)
			}
		})
	}
}

// TestResolveDriverImage 는 노드 커널에 맞는 검증 태그 이미지가 있으면 그것을, 없거나
// 검증 태그가 아니면 기본 설치 이미지를 돌려주는지 본다. spec.driver.version 이 비어 있으므로
// 버전 토큰 가드는 적용되지 않는다(하위호환 경로).
func TestResolveDriverImage(t *testing.T) {
	pol := &v1alpha1.DriverInstallPolicy{Spec: v1alpha1.DriverInstallPolicySpec{Driver: v1alpha1.DriverSpec{
		Image: "reg/kcloud/tenstorrent-driver-ds:2.11.0-v1",
		KernelImages: map[string]v1alpha1.KernelImageRef{
			"5.15.0-190-generic": "reg/kcloud/tenstorrent-driver-ds:2.11.0-k5.15.0-190-generic-v1",
			"6.8.0-138-generic":  "reg/kcloud/tenstorrent-driver-ds:not-a-verified-tag",
		},
	}}}
	cases := []struct{ name, kernel, want string }{
		{"커널 일치", "5.15.0-190-generic", "reg/kcloud/tenstorrent-driver-ds:2.11.0-k5.15.0-190-generic-v1"},
		{"커널 불일치는 기본 이미지", "5.15.0-171-generic", "reg/kcloud/tenstorrent-driver-ds:2.11.0-v1"},
		{"검증 태그 아님은 기본 이미지", "6.8.0-138-generic", "reg/kcloud/tenstorrent-driver-ds:2.11.0-v1"},
		{"빈 커널은 기본 이미지", "", "reg/kcloud/tenstorrent-driver-ds:2.11.0-v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _, _ := resolveDriverImage(pol, tc.kernel); got != tc.want {
				t.Fatalf("resolveDriverImage(%q) = %q, want %q", tc.kernel, got, tc.want)
			}
		})
	}
}

// TestResolveDriverImage_VersionGuard 는 spec.driver.version 이 설정됐을 때 커널 이미지 태그의
// 버전 토큰이 그와 다르면 커널 이미지를 쓰지 않고 기본 이미지로 되돌아가며 이유를 남기는지 본다.
func TestResolveDriverImage_VersionGuard(t *testing.T) {
	// tt: Tenstorrent, spec.driver.version 2.11.0.
	tt := &v1alpha1.DriverInstallPolicy{Spec: v1alpha1.DriverInstallPolicySpec{Driver: v1alpha1.DriverSpec{
		Version: "2.11.0",
		Image:   "reg/kcloud/tenstorrent-driver-ds:2.11.0-v1",
		KernelImages: map[string]v1alpha1.KernelImageRef{
			"5.15.0-190-generic": "reg/kcloud/tenstorrent-driver-ds:2.11.0-k5.15.0-190-generic-v1",
			"5.15.0-171-generic": "reg/kcloud/tenstorrent-driver-ds:2.11.1-k5.15.0-171-generic-v1",
			"6.8.0-138-generic":  "reg/kcloud/tenstorrent-driver-ds:justverified-v1",
		},
	}}}
	// rb: Rebellions, variant 태그. 계획 버전 2026.3.0 대 태그 2026.3.1 이 어긋나는 실제 사례.
	rb := &v1alpha1.DriverInstallPolicy{Spec: v1alpha1.DriverInstallPolicySpec{Driver: v1alpha1.DriverSpec{
		Version: "2026.3.0",
		Image:   "reg/r/rebellions-driver-ds:2026.3.0-v1",
		KernelImages: map[string]v1alpha1.KernelImageRef{
			"6.8.0-139-generic": "reg/r/rebellions-driver-ds:2026.3.0-rngd-k6.8.0-139-generic-v1",
			"6.8.0-138-generic": "reg/r/rebellions-driver-ds:2026.3.1-rngd-k6.8.0-138-generic-v1",
		},
	}}}
	// empty: KernelImages 가 비어 있는 정책.
	empty := &v1alpha1.DriverInstallPolicy{Spec: v1alpha1.DriverInstallPolicySpec{Driver: v1alpha1.DriverSpec{
		Version: "2.11.0",
		Image:   "reg/kcloud/tenstorrent-driver-ds:2.11.0-v1",
	}}}

	cases := []struct {
		name      string
		pol       *v1alpha1.DriverInstallPolicy
		kernel    string
		want      string
		wantSkip  bool
		wantEvent string
	}{
		{"버전 일치는 커널 이미지 선택", tt, "5.15.0-190-generic",
			"reg/kcloud/tenstorrent-driver-ds:2.11.0-k5.15.0-190-generic-v1", false, ""},
		{"버전 불일치는 기본 이미지", tt, "5.15.0-171-generic",
			"reg/kcloud/tenstorrent-driver-ds:2.11.0-v1", true, eventKernelImageRejected},
		{"검증 태그이나 버전 토큰 없음은 기본 이미지", tt, "6.8.0-138-generic",
			"reg/kcloud/tenstorrent-driver-ds:2.11.0-v1", true, eventKernelImageRejected},
		{"variant 버전 일치는 커널 이미지 선택", rb, "6.8.0-139-generic",
			"reg/r/rebellions-driver-ds:2026.3.0-rngd-k6.8.0-139-generic-v1", false, ""},
		{"variant 버전 불일치는 기본 이미지", rb, "6.8.0-138-generic",
			"reg/r/rebellions-driver-ds:2026.3.0-v1", true, eventKernelImageRejected},
		{"KernelImages 비면 기본 이미지", empty, "5.15.0-190-generic",
			"reg/kcloud/tenstorrent-driver-ds:2.11.0-v1", false, ""},
		{"빈 커널은 기본 이미지", tt, "",
			"reg/kcloud/tenstorrent-driver-ds:2.11.0-v1", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, skip, event := resolveDriverImage(tc.pol, tc.kernel)
			if got != tc.want {
				t.Fatalf("resolveDriverImage(%q) 이미지 = %q, want %q", tc.kernel, got, tc.want)
			}
			if (skip != "") != tc.wantSkip {
				t.Fatalf("resolveDriverImage(%q) skipReason = %q, wantSkip = %v", tc.kernel, skip, tc.wantSkip)
			}
			if event != tc.wantEvent {
				t.Fatalf("resolveDriverImage(%q) 이벤트 reason = %q, want %q", tc.kernel, event, tc.wantEvent)
			}
		})
	}
}

// TestHandleUpgradingJob_UsesResolvedImage 는 preflight 이 상태에 남긴 ResolvedImage 를
// install Job 이 그대로 싣는지, 그리고 previousImage 는 커널 토큰 없이 이전 버전 이미지로
// 캡처되는지 본다(reconstructPrevImage 는 variant 접미사와 prevVersion 만 쓴다).
func TestHandleUpgradingJob_UsesResolvedImage(t *testing.T) {
	// 커널 이미지의 빌드 개정을 일반 설치 이미지(-v16)와 **다르게** 둔다. 같게 두면 롤백 대상
	// 재구성이 어느 입력을 썼는지 결과로 구분되지 않아, 호출부가 resolvedImage 를 넘기도록
	// 되돌아가도 이 시험이 통과한다.
	const kernelImg = "reg/nv:590.48.01-k5.15.0-190-generic-v20"
	dus := makeJobDUS(v1alpha1.UpgradeStateUpgrading, verOld, verNew)
	// handleIdle 이 Upgrading 진입 전에 PreviousVersion 을 세팅한다(캡처의 소스).
	dus.Status.PreviousVersion = verOld
	// preflight 이 노드 커널로 고른 이미지를 상태에 남겼다고 가정한다.
	dus.Status.ResolvedImage = kernelImg
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip)

	req, _, err := sm.handleUpgradingJob(context.Background(), dus, dip)
	if err != nil {
		t.Fatalf("handleUpgradingJob 실패: %v", err)
	}
	if !req {
		t.Error("requeue 기대")
	}
	if dus.Status.State != v1alpha1.UpgradeStateValidating {
		t.Errorf("state=%q, want Validating", dus.Status.State)
	}
	var job batchv1.Job
	jn := naming.InstallJobName(jVendor, jModel, jNode)
	if err := sm.Get(context.Background(), types.NamespacedName{Name: jn, Namespace: driverjob.Namespace}, &job); err != nil {
		t.Fatalf("install Job 미생성: %v", err)
	}
	// install Job 은 policy.image 가 아니라 preflight 이 고른 resolvedImage 를 실어야 한다.
	if got := jobContainerImage(&job); got != kernelImg {
		t.Errorf("job image=%q, want resolvedImage %q", got, kernelImg)
	}
	// previousImage 는 커널 토큰 없이 이전 버전 이미지로 캡처된다 — 재구성 입력이
	// policy.Spec.Driver.Image(-v16)이므로 커널 이미지의 개정(-v20)이 결과에 섞이지 않는다.
	if dus.Status.PreviousImage != imgOld {
		t.Errorf("PreviousImage=%q, want %q", dus.Status.PreviousImage, imgOld)
	}
}

// TestHandleRollbackJob_KernelImagesWithoutPreviousImageReconstructs 는 kernelImages 정책에서
// 캡처된 previousImage 가 없어도 롤백이 거부되지 않고 이전 버전 DKMS 태그로 재구성해 진행하는지
// 본다. 재구성은 policy.Spec.Driver.Image(DKMS 이미지)에서 하므로 커널 토큰이 입력에 없고, 결과는
// 이전 버전 DKMS 태그(reg/nv:580.126.09-v16)로 존재한다.
func TestHandleRollbackJob_KernelImagesWithoutPreviousImageReconstructs(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRollback, verNew, verNew)
	dus.Status.PreviousVersion = verOld // 이전 버전은 있다
	dus.Status.PreviousImage = ""       // 캡처값 없음 — 재구성 경로로 간다
	dip := makeJobDIP(verNew, imgNew, true, true)
	// 커널별 이미지 정책 — 재구성은 policy.Spec.Driver.Image(imgNew)에서 하므로 무관하다.
	dip.Spec.Driver.KernelImages = map[string]v1alpha1.KernelImageRef{
		"5.15.0-190-generic": "reg/nv:590.48.01-k5.15.0-190-generic-v16",
	}
	sm := newUpgradeSMWithRecorder(dus, dip)

	if _, _, err := sm.handleRollbackJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRollbackJob 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateValidating {
		t.Errorf("state=%q, want Validating (kernelImages 정책도 DKMS 이전 이미지로 롤백)", dus.Status.State)
	}
	if dus.Status.DesiredVersion != verOld {
		t.Errorf("desiredVersion=%q, want %q (이전 버전)", dus.Status.DesiredVersion, verOld)
	}
	if dus.Status.RollbackAttempts != 1 {
		t.Errorf("rollbackAttempts=%d, want 1", dus.Status.RollbackAttempts)
	}
	var job batchv1.Job
	jn := naming.InstallJobName(jVendor, jModel, jNode)
	if err := sm.Get(context.Background(), types.NamespacedName{Name: jn, Namespace: driverjob.Namespace}, &job); err != nil {
		t.Fatalf("rollback Job 미생성: %v", err)
	}
	// 재구성된 롤백 대상 = 이전 버전 DKMS 태그(커널 토큰 없음).
	if got := jobContainerImage(&job); got != imgOld {
		t.Errorf("rollback job image=%q, want %q (이전 버전 DKMS 태그)", got, imgOld)
	}
}

// 정책에서 kernelImages 항목이 빠지면 Idle 에서도 status.ResolvedImage 가 기본 이미지로
// 돌아와야 한다. PreFlight 에서만 계산하면 사라진 태그가 status 에 계속 남는다.
func TestHandleIdleRefreshesResolvedImage(t *testing.T) {
	const (
		defaultImage  = "reg/kcloud/furiosa-driver-ds:v3"
		prebuiltImage = "reg/kcloud/furiosa-driver-ds:2026.3.1-rngd-k6.8.0-138-generic-v1"
		kernel        = "6.8.0-138-generic"
	)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "rngd-1"},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KernelVersion: kernel}},
	}

	t.Run("맵이 비면 기본 이미지로 돌아온다", func(t *testing.T) {
		dus := makeJobDUS(v1alpha1.UpgradeStateIdle, "2026.3.1", "2026.3.1")
		dus.Spec.NodeName = "rngd-1"
		dus.Status.ResolvedImage = prebuiltImage
		dip := makeJobDIP("2026.3.1", defaultImage, false, true)
		dip.Spec.Driver.TrackOnly = true // 설치 경로를 타지 않고 갱신만 보게 한다
		sm := newUpgradeSMWithRecorder(dus, dip, node)

		if _, _, err := sm.handleIdle(context.Background(), dus, dip); err != nil {
			t.Fatalf("handleIdle 실패: %v", err)
		}
		if dus.Status.ResolvedImage != defaultImage {
			t.Errorf("ResolvedImage=%q, want %q", dus.Status.ResolvedImage, defaultImage)
		}
	})

	t.Run("맵에 커널이 있으면 그 이미지로 갱신된다", func(t *testing.T) {
		dus := makeJobDUS(v1alpha1.UpgradeStateIdle, "2026.3.1", "2026.3.1")
		dus.Spec.NodeName = "rngd-1"
		dus.Status.ResolvedImage = ""
		dip := makeJobDIP("2026.3.1", defaultImage, false, true)
		dip.Spec.Driver.TrackOnly = true
		dip.Spec.Driver.KernelImages = map[string]v1alpha1.KernelImageRef{
			kernel: v1alpha1.KernelImageRef(prebuiltImage),
		}
		sm := newUpgradeSMWithRecorder(dus, dip, node)

		if _, _, err := sm.handleIdle(context.Background(), dus, dip); err != nil {
			t.Fatalf("handleIdle 실패: %v", err)
		}
		if dus.Status.ResolvedImage != prebuiltImage {
			t.Errorf("ResolvedImage=%q, want %q", dus.Status.ResolvedImage, prebuiltImage)
		}
	})

	t.Run("노드가 없으면 값을 그대로 둔다", func(t *testing.T) {
		dus := makeJobDUS(v1alpha1.UpgradeStateIdle, "2026.3.1", "2026.3.1")
		dus.Spec.NodeName = "사라진-노드"
		dus.Status.ResolvedImage = prebuiltImage
		dip := makeJobDIP("2026.3.1", defaultImage, false, true)
		dip.Spec.Driver.TrackOnly = true
		sm := newUpgradeSMWithRecorder(dus, dip)

		if _, _, err := sm.handleIdle(context.Background(), dus, dip); err != nil {
			t.Fatalf("handleIdle 실패: %v", err)
		}
		if dus.Status.ResolvedImage != prebuiltImage {
			t.Errorf("노드가 없으면 유지해야 한다: ResolvedImage=%q", dus.Status.ResolvedImage)
		}
	})
}

// TestReconstructPrevImage_AptKernelImageVersionLock 는 apt 인스톨러에서 사전 컴파일 커널
// 이미지가 버전에 종속됨을 고정한다. 커널 이미지(태그 <version>-k<kernel>-vN)는 태그에 박힌
// 버전 모듈만 담아 DRIVER_VERSION 으로 다른 버전을 설치하지 못한다. 되돌릴 버전이 다르면
// 재구성 불가("")로 돌려 호출부가 기본 이미지로 되돌아가게 하고, 같으면 그대로 쓴다.
func TestReconstructPrevImage_AptKernelImageVersionLock(t *testing.T) {
	const kernelImg = "reg/nv:590.48.01-k5.15.0-190-generic-v16" // 박힌 버전 = 590.48.01

	// 박힌 버전(590.48.01) != 되돌릴 버전(580.126.09) → 이 이미지로는 되돌릴 수 없다.
	if got := reconstructPrevImage(kernelImg, v1alpha1.DriverInstallerAPT, verOld, false); got != "" {
		t.Errorf("mismatch: reconstructPrevImage(%q, apt, %q)=%q, want \"\" (커널 이미지 재사용 금지)",
			kernelImg, verOld, got)
	}
	// 박힌 버전(590.48.01) == 되돌릴 버전(590.48.01) → 그 커널 이미지를 그대로 쓴다.
	if got := reconstructPrevImage(kernelImg, v1alpha1.DriverInstallerAPT, verNew, false); got != kernelImg {
		t.Errorf("match: reconstructPrevImage(%q, apt, %q)=%q, want %q (박힌 버전 일치 시 그대로)",
			kernelImg, verNew, got, kernelImg)
	}
}

// TestHandleUpgradingJob_AptKernelImageNotCaptured 는 apt 인스톨러에서 preflight 이 고른 사전
// 컴파일 커널 이미지가 이전 버전 이미지(PreviousImage)로 캡처되지 않음을 고정한다. 커널 이미지는
// 새 버전 모듈만 담아 이전 버전 설치에 쓸 수 없다 — 캡처되면 되돌리기 Job 이 그 이미지에 이전
// 버전 DRIVER_VERSION 을 실어 설치기가 거부하고 노드가 굳는다. 캡처는 비워 두고
// 되돌리기는 기본 이미지 재구성 경로로 가야 한다. 설치(forward) Job 은 커널 이미지를 그대로 쓴다.
func TestHandleUpgradingJob_AptKernelImageNotCaptured(t *testing.T) {
	const kernelImg = "reg/nv:590.48.01-k5.15.0-190-generic-v16" // 박힌 버전 = verNew
	dus := makeJobDUS(v1alpha1.UpgradeStateUpgrading, verOld, verNew)
	dus.Status.PreviousVersion = verOld // 되돌릴 대상 = 이전 버전
	dus.Status.ResolvedImage = kernelImg
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Spec.Driver.Installer = v1alpha1.DriverInstallerAPT
	sm := newUpgradeSMWithRecorder(dus, dip)

	if _, _, err := sm.handleUpgradingJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleUpgradingJob 실패: %v", err)
	}
	// 커널 이미지를 이전 버전 이미지로 캡처하면 안 된다(되돌리기가 깨진다).
	if dus.Status.PreviousImage == kernelImg {
		t.Errorf("PreviousImage=%q — 커널 이미지를 이전 버전 이미지로 캡처", dus.Status.PreviousImage)
	}
	// forward install Job 은 preflight 이 고른 커널 이미지를 그대로 싣는다(설치 경로 무변경).
	var job batchv1.Job
	jn := naming.InstallJobName(jVendor, jModel, jNode)
	if err := sm.Get(context.Background(), types.NamespacedName{Name: jn, Namespace: driverjob.Namespace}, &job); err != nil {
		t.Fatalf("install Job 미생성: %v", err)
	}
	if got := jobContainerImage(&job); got != kernelImg {
		t.Errorf("forward job image=%q, want %q (설치 경로는 커널 이미지 그대로)", got, kernelImg)
	}
}

// TestResolveDriverImage_Assembled 는 kernelImages 가 비어도 kernelImageBuildRevision 이
// 선언되면 태그 규약으로 이미지를 조립하는지, 맵 항목이 조립보다 우선하는지, 빌드 개정이
// 비면 조립하지 않는지, 조립 결과가 기존 검증을 통과하지 못하면 쓰지 않는지 본다.
// 이벤트 reason 도 함께 본다 — 거절된 이미지가 없는 상황에 거절 reason 이 붙으면 운영자가
// 있지도 않은 이미지를 찾는다.
func TestResolveDriverImage_Assembled(t *testing.T) {
	// on: 맵은 비고 빌드 개정만 선언한 정책. 조립 경로가 유일한 선택 수단이다.
	on := func() *v1alpha1.DriverInstallPolicy {
		return &v1alpha1.DriverInstallPolicy{Spec: v1alpha1.DriverInstallPolicySpec{Driver: v1alpha1.DriverSpec{
			Mode:                     "job",
			Version:                  "580.178.04",
			Image:                    "reg/kcloud/nvidia-driver-ds:v1",
			KernelImageBuildRevision: "v4",
		}}}
	}
	// off: 같은 정책에서 빌드 개정만 비운 것. 지금 동작(조립 없음)의 기준이다.
	off := on()
	off.Spec.Driver.KernelImageBuildRevision = ""
	// pinned: 맵에 그 커널 항목이 있는 정책. 맵이 조립을 이겨야 한다.
	pinned := on()
	pinned.Spec.Driver.KernelImages = map[string]v1alpha1.KernelImageRef{
		"5.15.0-190-generic": "reg/other/nvidia-driver-ds:580.178.04-k5.15.0-190-generic-v9",
	}
	// variant: variant 토막을 선언한 정책. Furiosa 계열 발행 규약이 이 모양이다.
	variant := on()
	variant.Spec.Driver.Version = "2026.3.1"
	variant.Spec.Driver.Image = "reg/kcloud/furiosa-driver-ds:v3"
	variant.Spec.Driver.KernelImageVariant = "rngd"
	variant.Spec.Driver.KernelImageBuildRevision = "v1"
	// aptVer: apt 개정 접미사를 단 버전. 조립한 태그의 버전 토큰이 "580.178.04-0" 으로 끊겨
	// spec.driver.version 과 달라지므로 기존 버전 대조가 그 태그를 거절한다.
	aptVer := on()
	aptVer.Spec.Driver.Version = "580.178.04-0ubuntu1"
	// noVer: 버전이 빈 하위호환 경로. 버전 토큰이 없으면 규약을 조립할 수 없고, 조립할 것이
	// 없으므로 경고도 내지 않는다(CRD CEL 이 이 조합을 입구에서 거절한다).
	noVer := on()
	noVer.Spec.Driver.Version = ""
	// oddVer: 태그에 쓸 수 없는 문자가 섞인 버전. 커널 토막과 대칭으로 거른다.
	oddVer := on()
	oddVer.Spec.Driver.Version = "1.0 beta"

	cases := []struct {
		name      string
		pol       *v1alpha1.DriverInstallPolicy
		kernel    string
		want      string
		wantSkip  bool
		wantEvent string
	}{
		{"맵이 비어도 조립한다", on(), "5.15.0-190-generic",
			"reg/kcloud/nvidia-driver-ds:580.178.04-k5.15.0-190-generic-v4", false, ""},
		{"다른 커널도 같은 규약으로 조립한다", on(), "6.8.0-138-generic",
			"reg/kcloud/nvidia-driver-ds:580.178.04-k6.8.0-138-generic-v4", false, ""},
		{"variant 를 선언하면 버전과 커널 사이에 끼운다", variant, "6.8.0-138-generic",
			"reg/kcloud/furiosa-driver-ds:2026.3.1-rngd-k6.8.0-138-generic-v1", false, ""},
		{"맵 항목이 조립보다 우선", pinned, "5.15.0-190-generic",
			"reg/other/nvidia-driver-ds:580.178.04-k5.15.0-190-generic-v9", false, ""},
		{"맵에 없는 커널은 조립으로 내려간다", pinned, "6.8.0-138-generic",
			"reg/kcloud/nvidia-driver-ds:580.178.04-k6.8.0-138-generic-v4", false, ""},
		{"빌드 개정이 비면 조립하지 않는다", off, "5.15.0-190-generic",
			"reg/kcloud/nvidia-driver-ds:v1", false, ""},
		{"조립 결과의 버전 토큰이 어긋나면 쓰지 않는다", aptVer, "5.15.0-190-generic",
			"reg/kcloud/nvidia-driver-ds:v1", true, eventKernelImageRejected},
		{"버전이 비면 조립도 경고도 없다", noVer, "5.15.0-190-generic",
			"reg/kcloud/nvidia-driver-ds:v1", false, ""},
		{"태그로 쓸 수 없는 커널은 조립하지 않는다", on(), "6.1.0-rpi7+custom",
			"reg/kcloud/nvidia-driver-ds:v1", true, eventKernelImageNotAssembled},
		{"태그로 쓸 수 없는 버전은 조립하지 않는다", oddVer, "5.15.0-190-generic",
			"reg/kcloud/nvidia-driver-ds:v1", true, eventKernelImageNotAssembled},
		{"빈 커널은 기본 이미지", on(), "", "reg/kcloud/nvidia-driver-ds:v1", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, skip, event := resolveDriverImage(tc.pol, tc.kernel)
			if got != tc.want {
				t.Fatalf("resolveDriverImage(%q) 이미지 = %q, want %q", tc.kernel, got, tc.want)
			}
			if (skip != "") != tc.wantSkip {
				t.Fatalf("resolveDriverImage(%q) skipReason = %q, wantSkip = %v", tc.kernel, skip, tc.wantSkip)
			}
			if event != tc.wantEvent {
				t.Fatalf("resolveDriverImage(%q) 이벤트 reason = %q, want %q", tc.kernel, event, tc.wantEvent)
			}
		})
	}
}

// TestAssembledTagMatchesVendorPublishConvention 은 차트 기본값에서 뜬 네 벤더 정책이 각자의
// 발행 규약과 같은 모양을 조립하는지 본다. variant 유무가 벤더마다 다르고 그 차이를 Go 가 아니라
// 정책이 받는다 — 값이 어긋나면 레지스트리에 없는 태그를 install Job 에 싣는다.
func TestAssembledTagMatchesVendorPublishConvention(t *testing.T) {
	const kernel = "5.15.0-190-generic"
	cases := []struct {
		vendor, version, image, variant, build, want string
	}{
		// NVIDIA·Tenstorrent 규약에는 variant 토막이 없다.
		{"nvidia", "580.126.09", "reg/kcloud/nvidia-driver-ds:v3", "", "v4",
			"reg/kcloud/nvidia-driver-ds:580.126.09-k5.15.0-190-generic-v4"},
		{"tenstorrent", "2.11.0", "reg/kcloud/tenstorrent-driver-ds:2.11.0-v1", "", "v1",
			"reg/kcloud/tenstorrent-driver-ds:2.11.0-k5.15.0-190-generic-v1"},
		// Furiosa 계열 규약은 "<pkgVersion>-<variant>-k<커널>-vN" 이다.
		{"rngd", "2026.3.1", "reg/kcloud/furiosa-driver-ds:v3", "rngd", "v1",
			"reg/kcloud/furiosa-driver-ds:2026.3.1-rngd-k5.15.0-190-generic-v1"},
		{"warboy", "1.9.9-3", "reg/kcloud/furiosa-driver-ds:v3", "warboy", "v1",
			"reg/kcloud/furiosa-driver-ds:1.9.9-3-warboy-k5.15.0-190-generic-v1"},
	}
	for _, tc := range cases {
		t.Run(tc.vendor, func(t *testing.T) {
			pol := &v1alpha1.DriverInstallPolicy{Spec: v1alpha1.DriverInstallPolicySpec{Driver: v1alpha1.DriverSpec{
				Mode: "job", Version: tc.version, Image: tc.image,
				KernelImageVariant: tc.variant, KernelImageBuildRevision: tc.build,
			}}}
			got, skip, _ := resolveDriverImage(pol, kernel)
			if skip != "" {
				t.Fatalf("조립이 거부됐다: %s", skip)
			}
			if got != tc.want {
				t.Fatalf("조립 결과 = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAssembledKernelImagePassesExistingGuards 는 조립한 참조가 맵 값과 같은 검증을
// 통과한다는 것을 그 검증 함수로 직접 확인한다. 조립이 검증을 우회하면 이 둘이 갈린다.
func TestAssembledKernelImagePassesExistingGuards(t *testing.T) {
	pol := &v1alpha1.DriverInstallPolicy{Spec: v1alpha1.DriverInstallPolicySpec{Driver: v1alpha1.DriverSpec{
		Mode:                     "job",
		Version:                  "1.9.9-3",
		Image:                    "reg/kcloud/furiosa-driver-ds:v3",
		KernelImageBuildRevision: "v1",
	}}}
	got, skip := assembleKernelImage(pol, "5.15.0-190-generic")
	if skip != "" {
		t.Fatalf("조립이 거부됐다: %s", skip)
	}
	if want := "reg/kcloud/furiosa-driver-ds:1.9.9-3-k5.15.0-190-generic-v1"; got != want {
		t.Fatalf("조립 결과 = %q, want %q", got, want)
	}
	if !isVerifiedBuildTag(got) {
		t.Errorf("조립한 태그가 검증 빌드 태그가 아니다: %q", got)
	}
	if v := extractKernelImageVersion(got); v != pol.Spec.Driver.Version {
		t.Errorf("조립한 태그의 버전 토큰 = %q, want %q", v, pol.Spec.Driver.Version)
	}
	if !isPrecompiledKernelImage(got) {
		t.Errorf("조립한 태그가 사전 컴파일 이미지로 식별되지 않는다: %q", got)
	}
}

// TestCaptureJobPreviousImage_UsesPolicyImageNotResolved 는 롤백 대상 이미지를 커널 이미지가 아니라
// 정책의 설치 이미지에서 만드는지 본다. 커널 이미지의 빌드 개정과 일반 설치 이미지의 개정은 서로
// 다른 수이므로, 커널 이미지에서 접미사를 떼면 레지스트리에 없는 태그가 나와 rollback Job 이
// ImagePullBackOff 로 앉고 노드가 cordon 에 고착된다. 조립 경로와 맵 경로 둘 다 본다 — 맵 경로는
// 사이트가 개정을 설치 이미지와 같게 적으면 두 입력의 결과가 우연히 같아져 결함이 가려진다.
// 그 우연까지 케이스로 적어 둔다.
func TestCaptureJobPreviousImage_UsesPolicyImageNotResolved(t *testing.T) {
	const kernel = "5.15.0-190-generic"
	// tenstorrent 차트 기본값. imageVersionBaked 라 롤백은 이전 버전 태그 재구성 경로다.
	ttPolicy := func() *v1alpha1.DriverInstallPolicy {
		return &v1alpha1.DriverInstallPolicy{Spec: v1alpha1.DriverInstallPolicySpec{Driver: v1alpha1.DriverSpec{
			Mode:              "job",
			Version:           "2.11.0",
			Image:             "reg/kcloud/tenstorrent-driver-ds:2.11.0-v1",
			Installer:         v1alpha1.DriverInstallerScript,
			ImageVersionBaked: true,
		}}}
	}
	assembled := ttPolicy()
	assembled.Spec.Driver.KernelImageBuildRevision = "v4"
	// 커널 이미지의 개정이 설치 이미지와 다른 경우. 발행 실제 예가 이 모양이다 — 커널 이미지
	// …-v4 와 같은 정책의 일반 설치 이미지 …-v1 은 서로 다른 개정이다.
	mappedDiff := ttPolicy()
	mappedDiff.Spec.Driver.KernelImages = map[string]v1alpha1.KernelImageRef{
		kernel: "reg/kcloud/tenstorrent-driver-ds:2.11.0-k5.15.0-190-generic-v9",
	}
	// 개정이 우연히 같은 경우. 결함이 가려지는 조합이라 값만 고정한다.
	mappedSame := ttPolicy()
	mappedSame.Spec.Driver.KernelImages = map[string]v1alpha1.KernelImageRef{
		kernel: "reg/kcloud/tenstorrent-driver-ds:2.11.0-k5.15.0-190-generic-v1",
	}
	// nvidia 차트 기본값. 비-baked 라 같은 이미지 + DRIVER_VERSION 으로 되돌린다.
	apt := &v1alpha1.DriverInstallPolicy{Spec: v1alpha1.DriverInstallPolicySpec{Driver: v1alpha1.DriverSpec{
		Mode:                     "job",
		Version:                  "580.178.04",
		Image:                    "reg/kcloud/nvidia-driver-ds:v1",
		Installer:                v1alpha1.DriverInstallerAPT,
		KernelImageBuildRevision: "v4",
	}}}

	cases := []struct {
		name, prevVersion, want string
		pol                     *v1alpha1.DriverInstallPolicy
		// inputMatters 는 커널 이미지를 입력으로 줬을 때 결과가 달라지는 조합인지다.
		inputMatters bool
	}{
		{"조립 경로는 일반 설치 이미지의 개정을 쓴다", "2.8.0",
			"reg/kcloud/tenstorrent-driver-ds:2.8.0-v1", assembled, true},
		{"맵 경로의 개정이 다르면 설치 이미지 쪽을 쓴다", "2.8.0",
			"reg/kcloud/tenstorrent-driver-ds:2.8.0-v1", mappedDiff, true},
		{"맵 경로의 개정이 같으면 결과가 하나다", "2.8.0",
			"reg/kcloud/tenstorrent-driver-ds:2.8.0-v1", mappedSame, false},
		{"비-baked 는 버전 비종속 설치 이미지로 되돌린다", "580.126.09",
			"reg/kcloud/nvidia-driver-ds:v1", apt, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 그 커널에서 커널 이미지가 실제로 선택되는지 먼저 확인한다 — 선택되지 않으면
			// 이 시험은 아무것도 증명하지 않는다.
			resolved, skip, _ := resolveDriverImage(tc.pol, kernel)
			if skip != "" || resolved == tc.pol.Spec.Driver.Image {
				t.Fatalf("커널 이미지가 선택되지 않았다: resolved=%q skip=%q", resolved, skip)
			}
			state := &v1alpha1.DriverUpgradeState{Status: v1alpha1.DriverUpgradeStateStatus{
				PreviousVersion: tc.prevVersion,
				ResolvedImage:   resolved,
			}}
			captureJobPreviousImage(state, tc.pol.Spec.Driver.Image,
				tc.pol.Spec.Driver.Installer, tc.pol.Spec.Driver.ImageVersionBaked)
			if state.Status.PreviousImage != tc.want {
				t.Fatalf("PreviousImage = %q, want %q", state.Status.PreviousImage, tc.want)
			}
			fromResolved := reconstructPrevImage(resolved, tc.pol.Spec.Driver.Installer,
				tc.prevVersion, tc.pol.Spec.Driver.ImageVersionBaked)
			if tc.inputMatters && fromResolved == tc.want {
				t.Errorf("커널 이미지를 입력으로 줘도 결과가 같다(%q) — 이 케이스가 입력 차이를 못 본다",
					fromResolved)
			}
			if !tc.inputMatters && fromResolved != tc.want {
				t.Errorf("개정이 같은 조합인데 결과가 갈렸다: %q != %q", fromResolved, tc.want)
			}
		})
	}
}

// TestResolveDriverImage_MapMissForDeclaredKernelsWarns 는 정책이 kernelImages 를 선언했는데
// 노드 커널 항목이 없으면(조립도 켜지지 않은 경우) 기본 이미지로 조용히 되돌아가지 않고
// KernelImageMissing 경고와 선언된 커널 목록을 담은 사유를 내는지 본다. 커널이 OS 업데이트로
// 바뀐 뒤 아무도 모르게 느린 DKMS 경로로 떨어지는 것을 드러내기 위한 가드다.
func TestResolveDriverImage_MapMissForDeclaredKernelsWarns(t *testing.T) {
	policy := &v1alpha1.DriverInstallPolicy{}
	policy.Spec.Driver.Image = "reg/nvidia-driver-ds:v4"
	policy.Spec.Driver.Version = "580.126.09"
	policy.Spec.Driver.KernelImages = map[string]v1alpha1.KernelImageRef{
		"5.15.0-171-generic": "reg/nvidia-driver-ds:580.126.09-k5.15.0-171-generic-v4",
	}
	img, reason, event := resolveDriverImage(policy, "5.15.0-191-generic")
	if img != policy.Spec.Driver.Image || reason == "" || event != eventKernelImageMissing {
		t.Fatalf("맵 미스가 조용히 지나간다: img=%s reason=%q event=%q", img, reason, event)
	}
	if !strings.Contains(reason, "5.15.0-171-generic") {
		t.Fatalf("사유에 선언된 커널 목록이 없다: %q", reason)
	}
}

// TestResolveDriverImage_NoKernelImagesStaysSilent 는 kernelImages 를 선언하지 않은 정책(맵이
// 비어 하위호환 경로를 타는 경우)은 경고를 내지 않는지 본다 — 선언한 적 없는 맵에 경고를 내면
// kernelImages 를 쓰지 않는 모든 정책이 PreFlight 마다 뜻 없는 이벤트를 쌓는다.
func TestResolveDriverImage_NoKernelImagesStaysSilent(t *testing.T) {
	policy := &v1alpha1.DriverInstallPolicy{}
	policy.Spec.Driver.Image = "reg/nvidia-driver-ds:v4"
	_, reason, event := resolveDriverImage(policy, "5.15.0-191-generic")
	if reason != "" || event != "" {
		t.Fatalf("kernelImages 를 선언하지 않은 정책에 경고가 난다: %q/%q", reason, event)
	}
}

// TestForeignSpecManagers 는 제3 관리자가 spec 필드를 쥔 경우만 골라내는지 본다. 제외 대상 셋:
// helm 자신(Apply·Update 둘 다 이름으로 제외), 제품 내부 관리자(CLI `kubectl-npu`, operator
// 바이너리 `manager`, 업그레이드 쓰기 경로 `kcloud-operator-upgrade`), metadata 만 쥔 관리자
// (kubectl-label). `kubectl-npu`·`manager` 는 FieldManager 명시 전 User-Agent 로 유도된 과거
// 이름이라 라이브 managedFields 에서 사라지면 제외 목록에서도 빼야 한다. 반대로 SSA 로 소유하는
// ArgoCD·Flux·`helm --server-side` 는 Operation=Apply 라, operation 한정이 남으면 전부 빠진다.
func TestForeignSpecManagers(t *testing.T) {
	const specFields = `{"f:spec":{"f:driver":{"f:kernelImages":{}}}}`
	mf := []metav1.ManagedFieldsEntry{
		{Manager: "helm", Operation: metav1.ManagedFieldsOperationApply, FieldsV1: &metav1.FieldsV1{Raw: []byte(specFields)}},
		{Manager: "helm", Operation: metav1.ManagedFieldsOperationUpdate, FieldsV1: &metav1.FieldsV1{Raw: []byte(specFields)}},
		{Manager: "kubectl-npu", Operation: metav1.ManagedFieldsOperationUpdate, FieldsV1: &metav1.FieldsV1{Raw: []byte(specFields)}},
		{Manager: "manager", Operation: metav1.ManagedFieldsOperationUpdate, FieldsV1: &metav1.FieldsV1{Raw: []byte(specFields)}},
		{Manager: "kcloud-operator-upgrade", Operation: metav1.ManagedFieldsOperationUpdate, FieldsV1: &metav1.FieldsV1{Raw: []byte(specFields)}},
		{Manager: "argocd-controller", Operation: metav1.ManagedFieldsOperationApply, FieldsV1: &metav1.FieldsV1{Raw: []byte(specFields)}},
		{Manager: "kubectl-patch", Operation: metav1.ManagedFieldsOperationUpdate, FieldsV1: &metav1.FieldsV1{Raw: []byte(specFields)}},
		{Manager: "kubectl-label", Operation: metav1.ManagedFieldsOperationUpdate, FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:labels":{}}}`)}},
	}
	want := []string{"argocd-controller", "kubectl-patch"}
	if got := foreignSpecManagers(mf); !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}
