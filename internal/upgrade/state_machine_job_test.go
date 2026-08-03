// ============================================================
// state_machine_job_test.go: WP-C-1 Job 모드 상태 핸들러 단위 테스트
// 상세: fake client 기반. handleIdle 초기설치 분기(Q1), handleUpgradingJob(생성),
//       handleValidatingJob(Job Complete+NDR→Uncordoning / 미완료→requeue /
//       실패→Rollback), handleRollbackJob(재-Job / 이전버전 부재→Failed),
//       daemonset 무영향(isJobMode) 검증.
// 생성일: 2026-07-16 | 수정일: 2026-08-07
// ============================================================

package upgrade

import (
	"context"
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

// ── handleIdle 초기 설치 (Q1) ────────────────────

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
// (Q1 self-heal — 재부팅/수동 rmmod 로 모듈 언로드된 job 모드 노드 복구).
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

// TrackOnly 정책은 driverLoaded=false self-heal 엣지에서도 install Job 을 트리거하지
// 않아야 한다(#15 TT 버전관리 편입 — DriverSpec.TrackOnly 계약: 설치 경로 없음).
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
	// handleIdle 이 Upgrading 진입 전에 PreviousVersion 을 세팅한다(Q2 캡처의 소스).
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
	// Q2: rollback 대상 이미지 캡처 (PreviousVersion=verOld + variant -v16)
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
	// 거부해 롤백이 무동작이 된다(2026-08-10 라이브).
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
//
// apt/script 인스톨러는 같은 이미지가 모든 드라이버 버전을 설치한다 — 목표 버전은 이미지
// 태그가 아니라 DRIVER_VERSION env 로 들어간다. 그래서 이미지만으로 Job 을 식별하면 지난
// 사이클이 남긴 완료된 Job 을 이번 사이클의 Job 으로 착각한다.
// 라이브 실측(2026-08-07 `.91` k8s-worker1): 595.84 사이클이 끝난 12초 뒤 580.173.02
// 사이클이 시작했고, 앞 사이클의 완료된 Job 이 TTL 안에 살아 있어 재사용됐다. 580 설치는
// 한 번도 생성되지 않은 채 validator 가 앞 Job 의 결과(595.84)를 10분간 채점했고 검증
// 예산 만료 → 불필요한 rollback → 불필요한 재부팅으로 이어졌다.

// makeAptJobDIP 는 apt 인스톨러 정책이다 — 모든 버전이 같은 이미지를 쓴다(라이브 구성).
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
		dipImage, installer, prevVersion, want string
	}{
		// ngc: 이미지가 드라이버를 담으므로 버전별 태그를 재구성한다.
		{imgNew, v1alpha1.DriverInstallerNGC, verOld, imgOld},
		{"reg/nv:1.7.8-v3", v1alpha1.DriverInstallerNGC, "1.7.7", "reg/nv:1.7.7-v3"},
		{"reg/nv:590.48.01", v1alpha1.DriverInstallerNGC, verOld, ""}, // variant 없음 → 재구성 불가
		{"", v1alpha1.DriverInstallerNGC, verOld, ""},                 // 이미지 없음
		{imgNew, v1alpha1.DriverInstallerNGC, "", ""},                 // prevVersion 없음
		// apt/script: 이미지는 버전과 무관하다. 태그를 갈아 끼우면 없는 이미지가 만들어져
		// rollback Job 이 ImagePullBackOff 로 앉는다 — 같은 이미지를 그대로 쓴다.
		{imgNew, v1alpha1.DriverInstallerAPT, verOld, imgNew},
		{"reg/nv:580.159.03-v179", v1alpha1.DriverInstallerAPT, "580.173.02", "reg/nv:580.159.03-v179"},
		{imgNew, v1alpha1.DriverInstallerScript, verOld, imgNew},
		{imgNew, v1alpha1.DriverInstallerAPT, "", ""}, // prevVersion 없음은 여전히 재구성 불가
	}
	for _, c := range cases {
		if got := reconstructPrevImage(c.dipImage, c.installer, c.prevVersion); got != c.want {
			t.Errorf("reconstructPrevImage(%q,%q,%q)=%q, want %q",
				c.dipImage, c.installer, c.prevVersion, got, c.want)
		}
	}
}

// TestRestartStaleDevicePluginPods 는 install Job 완료 시각 이전에 기동한 device-plugin
// Pod 만 삭제(재스캔 유도)하고, 완료 이후 기동 Pod 와 비-plugin Pod 는 보존함을 고정한다.
// device-plugin 재스캔 레이스(업그레이드 후 allocatable=0) 회귀 방지(#26).
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
// 그대로 쓰는 것을 고정한다. 버전으로 태그를 갈아 끼우면 레지스트리에 없는 이미지가 되고
// (실측: nvidia-driver-ds:580.173.02-v179 not found) Job 이 ImagePullBackOff 로 앉아
// 노드가 cordon 상태로 고착된다.
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
// 검증 예산이 만료되어 롤백으로 밀려나지 않는 것을 고정한다. 라이브 실측: apt 로 313MB 를
// 받는 도중 10분 예산이 끝나 설치가 중단되고, 이어진 롤백이 없는 이미지를 집어 노드가 굳었다.
func TestHandleValidatingJob_RunningJobDoesNotConsumeBudget(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateValidating, verOld, verNew)
	dus.Status.LastTransitionTime = metav1.NewTime(time.Now().Add(-30 * time.Minute))
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Spec.UpgradePolicy.ValidationTimeout = "10m"
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
	dip.Spec.UpgradePolicy.ValidationTimeout = "10m"
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
