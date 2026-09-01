// ============================================================
// driver_daemonset_controller_test.go: renderDriverDaemonSet 단위 테스트
// 상세: Phase C/D — operator pod anti-affinity, PreStop timeout 30s,
//        TerminationGracePeriodSeconds=60s 가 DS spec 에 박혀 있는지 검증
// 생성일: 2026-04-27 | 수정일: 2026-09-09
// ============================================================

package controller

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"

	v1alpha1 "kcloud-operator/api/v1alpha1"
)

// renderTestPolicy 는 renderDriverDaemonSet 입력용 최소 DIP 를 만든다.
func renderTestPolicy(vendor, model, version string) *v1alpha1.DriverInstallPolicy {
	return &v1alpha1.DriverInstallPolicy{
		Spec: v1alpha1.DriverInstallPolicySpec{
			Vendor: vendor,
			Model:  model,
			Driver: v1alpha1.DriverSpec{
				Version: version,
				Image:   "registry.example/driver:" + version,
				Mode:    "daemonset",
			},
		},
	}
}

// hasFuriosaAuthMount 는 DS pod spec 에 furiosa-auth secret 볼륨/마운트가 존재하는지 반환한다.
func hasFuriosaAuthMount(ds *appsv1.DaemonSet) bool {
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.Name == "furiosa-auth" {
			return true
		}
	}
	return false
}

// TestRenderDriverDaemonSet_FuriosaAuthWarboyOnly 는 furiosa-apt-auth secret 마운트가
// Warboy(사설 repo 인증 필수)에만 부착되고 RNGD(공개 repo)에는 부착되지 않음을 검증한다.
// 회귀 시나리오: RNGD 가 warboy 전제를 상속받아 secret 부재 클러스터에서 파드 스케줄 실패
// (service 클러스터 실측 버그). Warboy 는 하위호환으로 마운트 유지.
func TestRenderDriverDaemonSet_FuriosaAuthWarboyOnly(t *testing.T) {
	cases := []struct {
		name      string
		vendor    string
		model     string
		wantMount bool
	}{
		{"warboy has auth", "furiosa", "warboy", true},
		{"furiosa empty model = warboy", "furiosa", "", true},
		{"rngd no auth (public repo)", "furiosa", "rngd", false},
		{"rngd mixed case no auth", "Furiosa", "RNGD", false},
		{"nvidia no auth", "nvidia", "generic", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := renderDriverDaemonSet(renderTestPolicy(tc.vendor, tc.model, "1.0.0"))
			if got := hasFuriosaAuthMount(ds); got != tc.wantMount {
				t.Errorf("furiosa-auth 마운트=%v, want %v (vendor=%s model=%s)",
					got, tc.wantMount, tc.vendor, tc.model)
			}
		})
	}
}

// TestRenderDriverDaemonSet_AntiAffinityWithOperator 는 driver pod 가 operator pod 와
// 같은 노드에 spawn 되지 않도록 anti-affinity 가 적용되었는지 검증한다.
// 회귀 시나리오: operator pod 가 driver pod 와 같은 노드에 떠 있다가 노드 reboot
// 시 둘 다 사망 → driver-upgrading 라벨이 영구 stuck 되는 경로를 차단.
func TestRenderDriverDaemonSet_AntiAffinityWithOperator(t *testing.T) {
	ds := renderDriverDaemonSet(renderTestPolicy("nvidia", "generic", "590.48.01"))

	if ds.Spec.Template.Spec.Affinity == nil ||
		ds.Spec.Template.Spec.Affinity.PodAntiAffinity == nil {
		t.Fatalf("PodAntiAffinity 미설정 — operator pod 와의 노드 공유 차단 안 됨")
	}
	preferred := ds.Spec.Template.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(preferred) == 0 {
		t.Fatalf("PreferredDuringScheduling 항목 0 — anti-affinity 가 아무 효과 없음")
	}
	term := preferred[0].PodAffinityTerm
	if term.TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("TopologyKey=%q, expected kubernetes.io/hostname", term.TopologyKey)
	}
	if term.LabelSelector == nil ||
		term.LabelSelector.MatchLabels["app.kubernetes.io/name"] != "kcloud-operator" {
		t.Errorf("operator label selector 미일치: %+v", term.LabelSelector)
	}
	if term.LabelSelector.MatchLabels["app.kubernetes.io/component"] != "controller" {
		t.Errorf("operator component label 미일치: %+v", term.LabelSelector.MatchLabels)
	}
}

// TestRenderDriverDaemonSet_TerminationGrace 는 PreStop 의 rmmod hang 시에도
// kubelet 이 일정 시간 안에 강제 종료할 수 있도록 grace 가 설정되었는지 검증한다.
func TestRenderDriverDaemonSet_TerminationGrace(t *testing.T) {
	ds := renderDriverDaemonSet(renderTestPolicy("nvidia", "generic", "590.48.01"))

	if ds.Spec.Template.Spec.TerminationGracePeriodSeconds == nil {
		t.Fatalf("TerminationGracePeriodSeconds 미설정 — PreStop hang 시 무한 대기 위험")
	}
	got := *ds.Spec.Template.Spec.TerminationGracePeriodSeconds
	if got < 30 || got > 120 {
		t.Errorf("TerminationGracePeriodSeconds=%d (기대: 30~120)", got)
	}
}

// TestRenderDriverDaemonSet_PreStopTimeout 는 PreStop 명령이 timeout 으로 감싸여
// rmmod hang 시 자동 종료되는지 검증한다. nvidia/furiosa/RNGD 벤더별로 확인.
func TestRenderDriverDaemonSet_PreStopTimeout(t *testing.T) {
	cases := []struct {
		vendor string
		model  string
	}{
		{"nvidia", "generic"},
		{"furiosa", "warboy"},
		{"furiosa", "rngd"},
	}
	for _, tc := range cases {
		t.Run(tc.vendor+"/"+tc.model, func(t *testing.T) {
			ds := renderDriverDaemonSet(renderTestPolicy(tc.vendor, tc.model, "1.0.0"))
			if len(ds.Spec.Template.Spec.Containers) == 0 {
				t.Fatalf("driver 컨테이너 없음")
			}
			lc := ds.Spec.Template.Spec.Containers[0].Lifecycle
			if lc == nil || lc.PreStop == nil || lc.PreStop.Exec == nil {
				t.Fatalf("PreStop exec 핸들러 미설정")
			}
			cmd := strings.Join(lc.PreStop.Exec.Command, " ")
			if !strings.Contains(cmd, "timeout 30") {
				t.Errorf("PreStop 에 timeout 미적용: %q", cmd)
			}
			// rmmod 실패가 PreStop 자체 실패로 전파되어 grace 종료되지 않게 || true 보장
			if !strings.Contains(cmd, "|| true") {
				t.Errorf("PreStop 종료 코드 무시 (|| true) 누락: %q", cmd)
			}
		})
	}
}
