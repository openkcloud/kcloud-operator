// ============================================================
// state_machine_job_test.go: WP-C-1 Job 모드 상태 핸들러 단위 테스트
// 상세: fake client 기반. handleIdle 초기설치 분기(Q1), handleUpgradingJob(생성),
//       handleValidatingJob(Job Complete+NDR→Uncordoning / 미완료→requeue /
//       실패→Rollback), handleRollbackJob(재-Job / 이전버전 부재→Failed),
//       daemonset 무영향(isJobMode) 검증.
// 생성일: 2026-07-16
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
			Driver: v1alpha1.DriverSpec{Version: version, Image: image, Mode: "job"},
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
		dipImage, prevVersion, want string
	}{
		{imgNew, verOld, imgOld},
		{"reg/nv:1.7.8-v3", "1.7.7", "reg/nv:1.7.7-v3"},
		{"reg/nv:590.48.01", verOld, ""}, // variant 없음 → 재구성 불가
		{"", verOld, ""},                 // 이미지 없음
		{imgNew, "", ""},                 // prevVersion 없음
	}
	for _, c := range cases {
		if got := reconstructPrevImage(c.dipImage, c.prevVersion); got != c.want {
			t.Errorf("reconstructPrevImage(%q,%q)=%q, want %q", c.dipImage, c.prevVersion, got, c.want)
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
