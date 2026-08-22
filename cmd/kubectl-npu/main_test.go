// main_test.go: kubectl-npu 사람용 텍스트 출력 포맷 테스트
// 상세: 같은 노드에 붙은 장치 여러 대가 표에서 서로 구분되는지 본다(라이브에서 동일 행 중복 관측).
//
//	describe 명령의 렌더·인자 파싱도 여기서 시험한다.
//
// 생성일: 2026-08-11 | 수정일: 2026-08-24
package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"kcloud-operator/pkg/npuctl"
)

// captureStdout 는 f 가 stdout 에 쓴 내용을 문자열로 돌려준다.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	f()
	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close write pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

// TestPrintStatusText_DistinguishesDevicesOnSameNode 는 벤더·모델·버전이 모두 같고 PCI 주소만
// 다른 두 장치가 표에서 다른 행으로 읽히는지 본다. 라이브(2026-08-11)에서 A30 과 A2 가 완전히
// 동일한 두 줄로 나와 구분이 불가능했다.
func TestPrintStatusText_DistinguishesDevicesOnSameNode(t *testing.T) {
	status := &npuctl.ClusterStatus{
		Nodes: []npuctl.NodeStatus{{
			Name: "k8s-worker1",
			Devices: []npuctl.DeviceStatus{
				{
					Vendor: "nvidia", Product: "nvidia/generic", Count: 1,
					DriverLoaded: true, DriverVersion: "580.173.02",
					PCIeAddress: "0000:18:00.0",
				},
				{
					Vendor: "nvidia", Product: "nvidia/generic", Count: 1,
					DriverLoaded: true, DriverVersion: "580.173.02",
					PCIeAddress: "0000:86:00.0",
				},
			},
		}},
	}

	out := captureStdout(t, func() { printStatusText(status) })

	var rows []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "k8s-worker1") {
			rows = append(rows, line)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("장치 2대에 대해 행 2개가 나와야 함, got %d:\n%s", len(rows), out)
	}
	if rows[0] == rows[1] {
		t.Errorf("같은 노드의 두 장치 행이 동일해 구분 불가:\n%q", rows[0])
	}
	for i, pci := range []string{"0000:18:00.0", "0000:86:00.0"} {
		if !strings.Contains(rows[i], pci) {
			t.Errorf("행 %d 에 PCI 주소 %q 가 없음: %q", i, pci, rows[i])
		}
	}
}

// TestPrintStatusText_NoteShowsExclusionReason 는 배제된 노드의 행에만 NOTE 가 붙고
// 배제되지 않은 노드에는 배지도 이유도 안 붙는지 본다. operator 가 붙인 라벨을 옮긴
// 값이므로 여기서 사유를 다시 판정하지 않는다.
func TestPrintStatusText_NoteShowsExclusionReason(t *testing.T) {
	status := &npuctl.ClusterStatus{Nodes: []npuctl.NodeStatus{
		{
			Name: "k8s-master", Excluded: true, ExcludedReason: "control-plane",
			Devices: []npuctl.DeviceStatus{
				{Product: "nvidia/generic", PCIeAddress: "0000:01:00.0",
					Count: 1, DriverLoaded: true, DriverVersion: "580.159.03"},
			},
		},
		{
			Name: "k8s-worker1",
			Devices: []npuctl.DeviceStatus{
				{Product: "nvidia/a30", PCIeAddress: "0000:18:00.0",
					Count: 1, DriverLoaded: true, DriverVersion: "580.173.02"},
			},
		},
	}}
	out := captureStdout(t, func() { printStatusText(status) })

	var masterLine, workerLine string
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(ln, "k8s-master"):
			masterLine = ln
		case strings.Contains(ln, "k8s-worker1"):
			workerLine = ln
		}
	}
	if !strings.Contains(masterLine, "제외됨(control-plane)") {
		t.Errorf("배제된 노드 행에 사유가 없음: %q", masterLine)
	}
	if strings.Contains(workerLine, "제외됨") {
		t.Errorf("배제되지 않은 노드 행에 제외 표시가 붙음: %q", workerLine)
	}
}

// TestPrintStatusText_ShowsExclusionForNodeWithoutDevices 는 가속기 없는 배제 노드가 표에
// 남는지 본다. 배제 표시를 장치 루프 안에만 두면 status.devices 가 빈 노드는 행이 0개라
// 라벨이 붙어 있어도 화면에서 사라진다 — GPU 없는 control-plane 이 일반적 형상이다.
// 배제 아닌 장치 0개 노드는 기존대로 행을 내지 않는다.
func TestPrintStatusText_ShowsExclusionForNodeWithoutDevices(t *testing.T) {
	status := &npuctl.ClusterStatus{Nodes: []npuctl.NodeStatus{
		{Name: "k8s-master", Excluded: true, ExcludedReason: "control-plane"},
		{Name: "k8s-idle"},
	}}
	out := captureStdout(t, func() { printStatusText(status) })

	var masterLine string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "k8s-master") {
			masterLine = ln
		}
	}
	if !strings.Contains(masterLine, "제외됨(control-plane)") {
		t.Errorf("장치 0개 배제 노드가 표에서 사라짐: %q", masterLine)
	}
	if strings.Contains(out, "k8s-idle") {
		t.Error("배제 아닌 장치 0개 노드에 행이 생김 — 기존 동작이 바뀌었다")
	}
}

// TestPrintStatusText_OmitsEmptyReasonParens 는 사유가 빈 배제 노드에 빈 괄호(`제외됨()`)를
// 내지 않는지 본다 — 사유가 있는 줄 알게 만든다.
func TestPrintStatusText_OmitsEmptyReasonParens(t *testing.T) {
	status := &npuctl.ClusterStatus{Nodes: []npuctl.NodeStatus{
		{Name: "k8s-master", Excluded: true},
	}}
	out := captureStdout(t, func() { printStatusText(status) })
	if strings.Contains(out, "제외됨()") {
		t.Errorf("사유가 빈데 빈 괄호를 냈다: %q", out)
	}
	if !strings.Contains(out, "제외됨") {
		t.Errorf("배제 표시 자체가 사라졌다: %q", out)
	}
}

// 라이브(2026-08-11)에서 tenstorrent/blackhole-p150(26자)이 15자 열을 11자 넘겨
// 그 행부터 PCI 이후가 전부 밀렸다. 표를 눈으로 훑는 것이 이 명령의 용도이므로
// 한 행이 어긋나면 열을 따라갈 수 없다. 헤더와 모든 데이터 행에서 각 열의 시작
// 위치가 같은지 본다 — 폭을 하드코딩으로 되돌리면 이 단언이 걸린다.
func TestPrintStatusText_ColumnsAlignWithLongestDeviceName(t *testing.T) {
	status := &npuctl.ClusterStatus{Nodes: []npuctl.NodeStatus{
		{Name: "k8s-worker3", Devices: []npuctl.DeviceStatus{
			{Product: "tenstorrent/blackhole-p150", PCIeAddress: "0000:af:00.0",
				Count: 1, DriverLoaded: true, DriverVersion: "2.8.0"},
		}},
		{Name: "rngd-1.cluster.local", Devices: []npuctl.DeviceStatus{
			{Product: "furiosa/rngd", PCIeAddress: "0000:27:00.0",
				Count: 1, DriverLoaded: true, DriverVersion: "2026.3.0"},
		}},
		{Name: "k8s-worker1", Devices: []npuctl.DeviceStatus{
			{Product: "nvidia/a30", PCIeAddress: "0000:18:00.0",
				Count: 1, DriverLoaded: true, DriverVersion: "580.173.02"},
		}},
	}}
	out := captureStdout(t, func() { printStatusText(status) })

	var section []string
	in := false
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "=== NodeDeviceReports ===") {
			in = true
			continue
		}
		if in {
			if strings.TrimSpace(ln) == "" {
				break
			}
			section = append(section, ln)
		}
	}
	if len(section) < 4 {
		t.Fatalf("장치 절이 헤더 + 3행이어야 한다: %q", section)
	}

	// 헤더의 PCI 열 시작 위치를 기준으로 삼는다. 데이터 행의 PCI 주소도 같은 자리에서
	// 시작해야 한다 — 앞 열이 넘치면 그 행만 오른쪽으로 밀린다.
	want := strings.Index(section[0], "PCI")
	if want <= 0 {
		t.Fatalf("헤더에 PCI 열이 없다: %q", section[0])
	}
	for _, ln := range section[1:] {
		got := strings.Index(ln, "0000:")
		if got != want {
			t.Errorf("PCI 열이 %d 에서 시작해야 하는데 %d 다 — 행: %q", want, got, ln)
		}
	}
}

// TestRenderNodeDetail_없는값은대시 는 health·evidence·policy 가 없을 때 렌더가 값을
// 지어내지 않고 "—" 로 표시하는지 본다.
func TestRenderNodeDetail_없는값은대시(t *testing.T) {
	d := &npuctl.NodeDetail{Name: "k8s-worker1"}
	out := renderNodeDetail(d)
	for _, want := range []string{"k8s-worker1", "HEALTH", "—"} {
		if !strings.Contains(out, want) {
			t.Errorf("출력에 %q 가 없다:\n%s", want, out)
		}
	}
	for _, bad := range []string{"Healthy", "Verified"} {
		if strings.Contains(out, bad) {
			t.Errorf("없는 값을 지어냈다(%q):\n%s", bad, out)
		}
	}
}

// TestRenderNodeDetail_배제사유표시 는 배제된 노드의 사유가 NODE 줄에 붙는지 본다.
func TestRenderNodeDetail_배제사유표시(t *testing.T) {
	d := &npuctl.NodeDetail{Name: "k8s-master", Excluded: true, ExcludedReason: "control-plane"}
	out := renderNodeDetail(d)
	if !strings.Contains(out, "제외됨(control-plane)") {
		t.Errorf("배제 표시가 없다:\n%s", out)
	}
}

// TestRenderNodeDetail_광고량은규칙으로가른다 는 ADVERTISED 가 cpu/memory 등 비가속기
// 리소스를 걸러내고 "/" 포함 가속기 리소스만 내는지 본다.
func TestRenderNodeDetail_광고량은규칙으로가른다(t *testing.T) {
	d := &npuctl.NodeDetail{
		Name: "k8s-worker1",
		Allocatable: map[string]string{
			"cpu":            "32",
			"memory":         "128Gi",
			"pods":           "110",
			"hugepages-2Mi":  "0",
			"nvidia.com/gpu": "2",
		},
	}
	out := renderNodeDetail(d)
	if !strings.Contains(out, "nvidia.com/gpu") || !strings.Contains(out, "2") {
		t.Errorf("가속기 리소스가 안 보인다:\n%s", out)
	}
	for _, bad := range []string{"cpu", "memory", "pods", "hugepages"} {
		if strings.Contains(out, bad) {
			t.Errorf("비가속기 리소스가 새어 나왔다(%q):\n%s", bad, out)
		}
	}
}

// TestParseDescribeArgs_노드아니면실패 는 첫 인자가 "node" 가 아니면 ok=false 인지 본다.
func TestParseDescribeArgs_노드아니면실패(t *testing.T) {
	if _, _, ok := parseDescribeArgs([]string{"pod", "foo"}); ok {
		t.Error("node 가 아닌 서브커맨드에 ok=true 를 냈다")
	}
}

// TestParseDescribeArgs_이름생략하면전체노드 는 "node" 만 주면(이름 생략) 전체 노드
// 조회로 받아들이는지 본다 — 서브커맨드("node") 자체가 없으면 여전히 실패다.
func TestParseDescribeArgs_이름생략하면전체노드(t *testing.T) {
	name, _, ok := parseDescribeArgs([]string{"node"})
	if !ok {
		t.Fatal("이름 생략에 ok=false 를 냈다 — 전체 노드 조회가 안 된다")
	}
	if name != "" {
		t.Errorf("이름 생략인데 name=%q", name)
	}
	if _, _, ok := parseDescribeArgs(nil); ok {
		t.Error("node 서브커맨드 자체가 없는데 ok=true 를 냈다")
	}
}

// TestParseDescribeArgs_이름생략플래그만 은 "node --json"(이름 생략 + 플래그)에서도
// 전체 노드 + JSON 이 함께 파싱되는지 본다.
func TestParseDescribeArgs_이름생략플래그만(t *testing.T) {
	name, asJSON, ok := parseDescribeArgs([]string{"node", "--json"})
	if !ok || name != "" || !asJSON {
		t.Errorf("got name=%q asJSON=%v ok=%v, want name=\"\" asJSON=true ok=true", name, asJSON, ok)
	}
}

// TestParseDescribeArgs_이름과JSON플래그 는 정상 인자에서 이름과 --json 플래그를
// 제대로 뽑는지 본다.
func TestParseDescribeArgs_이름과JSON플래그(t *testing.T) {
	name, asJSON, ok := parseDescribeArgs([]string{"node", "k8s-worker1", "--json"})
	if !ok || name != "k8s-worker1" || !asJSON {
		t.Errorf("got name=%q asJSON=%v ok=%v", name, asJSON, ok)
	}
	name, asJSON, ok = parseDescribeArgs([]string{"node", "k8s-worker1"})
	if !ok || name != "k8s-worker1" || asJSON {
		t.Errorf("got name=%q asJSON=%v ok=%v", name, asJSON, ok)
	}
}

// TestRenderNodeDetails_실패노드는건너뛰고빈줄은실제출력블록수를따른다 는 cmdDescribe
// 전체노드 경로가 하는 것과 같은 전제(조회 실패 노드는 details 목록에서 이미 걸러짐)로
// renderNodeDetails 를 시험한다. 리뷰에서 지적된 결함 재발 방지용 — 예전엔 루프 인덱스
// `i > 0` 을 그대로 빈 줄 판정에 썼는데, names=[실패, ok, ok] 처럼 앞쪽 노드가 조회에
// 실패하면 실제로 처음 찍히는 블록 앞에도 빈 줄이 붙었다. 여기선 애초에 실패한 노드가
// details 슬라이스에 들어오지 않으므로 그 버그가 구조적으로 재발할 수 없다.
func TestRenderNodeDetails_실패노드는건너뛰고빈줄은실제출력블록수를따른다(t *testing.T) {
	// 빈 목록: 출력 없음.
	if out := renderNodeDetails(nil); out != "" {
		t.Errorf("빈 목록인데 출력이 났다: %q", out)
	}

	// 노드 하나: 앞뒤 빈 줄 없음.
	one := renderNodeDetails([]*npuctl.NodeDetail{{Name: "n1"}})
	if strings.HasPrefix(one, "\n") || strings.HasSuffix(one, "\n\n") {
		t.Errorf("노드 하나인데 여분의 빈 줄이 있다: %q", one)
	}
	if strings.Count(one, "\n\n") != 0 {
		t.Errorf("노드 하나인데 블록 구분 빈 줄이 났다: %q", one)
	}

	// 목록 조회와 상세 조회 사이 경합으로 첫 노드(n0)가 실패해 details 에서 빠지고
	// n1·n2 만 남은 상황을 재현 — cmdDescribe 가 채우는 details 와 동일한 모양이다.
	details := []*npuctl.NodeDetail{{Name: "n1"}, {Name: "n2"}}
	out := renderNodeDetails(details)
	if strings.HasPrefix(out, "\n") {
		t.Errorf("첫 노드가 실패해도 실제 첫 출력 블록 앞엔 빈 줄이 없어야 한다: %q", out)
	}
	n1Block := renderNodeDetail(details[0])
	n2Block := renderNodeDetail(details[1])
	want := n1Block + "\n" + n2Block
	if out != want {
		t.Errorf("got:\n%q\nwant:\n%q", out, want)
	}
}

// TestRenderNodeDetail_Health사유없으면빈괄호없음 은 Reason 이 빈 Health 에서 "Healthy ()" 같은
// 빈 괄호를 내지 않는지 본다. 라이브 두 노드 모두 이 상태다(리뷰에서 지적).
func TestRenderNodeDetail_Health사유없으면빈괄호없음(t *testing.T) {
	d := &npuctl.NodeDetail{
		Name:   "k8s-worker1",
		Health: &npuctl.HealthView{State: "Healthy", Reason: "", AllocationAllowed: true},
	}
	out := renderNodeDetail(d)
	if strings.Contains(out, "()") {
		t.Errorf("사유가 빈데 빈 괄호를 냈다:\n%s", out)
	}
	if !strings.Contains(out, "Healthy") {
		t.Errorf("HEALTH 상태가 안 보인다:\n%s", out)
	}
}

// TestRenderNodeDetail_Evidence식별자없음은명시 는 Vendor·Level 이 둘 다 비고 ExpiresAt 만
// 있는 evidence(라이브 k8s-master 실재 상태)가 라벨 없이 만료일만 흘리지 않고
// "식별자 없음" 으로 명시되는지 본다.
func TestRenderNodeDetail_Evidence식별자없음은명시(t *testing.T) {
	d := &npuctl.NodeDetail{
		Name:     "k8s-master",
		Evidence: []npuctl.EvidenceView{{Vendor: "", Level: "", ExpiresAt: "2026-08-04T15:23:46+09:00"}},
	}
	out := renderNodeDetail(d)
	if !strings.Contains(out, "식별자 없음") {
		t.Errorf("식별자 없는 evidence 가 명시되지 않았다:\n%s", out)
	}
	if !strings.Contains(out, "2026-08-04T15:23:46+09:00") {
		t.Errorf("만료 시각이 표에서 사라졌다:\n%s", out)
	}
}

// TestRenderNodeDetail_Evidence식별자없음표열정렬 은 VENDOR·LEVEL 이 둘 다 빈
// evidence 때문에 EXPIRES 열 위치가 실제 vendor·level 이 있는 표와 달라지지
// 않는지 본다. 리뷰 지적 — 이전엔 "식별자 없음"(16바이트)을 VENDOR 칸에 그대로
// 넣어 renderTable 이 폭을 재는 len()(바이트 수)이 헤더("VENDOR", 6바이트)보다
// 훨씬 커졌고, 그 결과 EXPIRES 열이 실제 vendor 가 있는 표보다 훨씬 오른쪽에서
// 시작했다(k8s-master·k8s-worker3 실측). "—" 로 바꾼 지금은 두 표의 EXPIRES
// 열이 같은 자리에서 시작해야 한다.
func TestRenderNodeDetail_Evidence식별자없음표열정렬(t *testing.T) {
	withVendor := renderNodeDetail(&npuctl.NodeDetail{
		Name:     "k8s-worker1",
		Evidence: []npuctl.EvidenceView{{Vendor: "nvidia", Level: "L1", ExpiresAt: "2026-08-04T15:23:46+09:00"}},
	})
	withoutVendor := renderNodeDetail(&npuctl.NodeDetail{
		Name:     "k8s-master",
		Evidence: []npuctl.EvidenceView{{Vendor: "", Level: "", ExpiresAt: "2026-08-04T15:23:46+09:00"}},
	})
	headerLine := func(out string) string {
		for _, ln := range strings.Split(out, "\n") {
			if strings.Contains(ln, "VENDOR") && strings.Contains(ln, "EXPIRES") {
				return ln
			}
		}
		return ""
	}
	h1, h2 := headerLine(withVendor), headerLine(withoutVendor)
	if h1 == "" || h2 == "" {
		t.Fatalf("EVIDENCE 헤더를 못 찾았다: %q / %q", h1, h2)
	}
	pos1, pos2 := strings.Index(h1, "EXPIRES"), strings.Index(h2, "EXPIRES")
	if pos1 != pos2 {
		t.Errorf("식별자 없는 evidence 때문에 EXPIRES 열 위치가 달라졌다: 실제 vendor=%d, 식별자 없음=%d", pos1, pos2)
	}
}

// TestRenderNodeDetail_장치표에헤더와정렬이있다 는 DEVICES 절이 PRODUCT/PCI 헤더를
// 갖고, 긴 제품명이 있어도 PCI 열이 모든 행에서 같은 자리에 오는지 본다.
func TestRenderNodeDetail_장치표에헤더와정렬이있다(t *testing.T) {
	d := &npuctl.NodeDetail{
		Name: "k8s-worker1",
		Devices: []npuctl.DeviceStatus{
			{
				Product: "tenstorrent/blackhole-p150", PCIeAddress: "0000:af:00.0",
				Count: 1, DriverLoaded: true, DriverVersion: "2.8.0",
			},
			{
				Product: "nvidia/a30", PCIeAddress: "0000:18:00.0",
				Count: 1, DriverLoaded: true, DriverVersion: "580.173.02",
			},
		},
	}
	out := renderNodeDetail(d)
	var section []string
	in := false
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "DEVICES:") {
			in = true
			continue
		}
		if in {
			if ln == "" || !strings.HasPrefix(ln, "  ") {
				break
			}
			section = append(section, ln)
		}
	}
	if len(section) != 3 {
		t.Fatalf("헤더 + 데이터 2행이어야 하는데 %d 줄: %q", len(section), section)
	}
	want := strings.Index(section[0], "PCI")
	if want <= 0 {
		t.Fatalf("헤더에 PCI 열이 없다: %q", section[0])
	}
	for _, ln := range section[1:] {
		if got := strings.Index(ln, "0000:"); got != want {
			t.Errorf("PCI 열이 %d 에서 시작해야 하는데 %d 다: %q", want, got, ln)
		}
	}
}

// TestRenderNodeDetail_Health장치행이표다 는 HEALTH 요약 줄은 산문으로 남고
// 그 아래 장치별 행이 PCI/STATE/REASON 헤더 있는 표인지 본다.
func TestRenderNodeDetail_Health장치행이표다(t *testing.T) {
	d := &npuctl.NodeDetail{
		Name: "k8s-worker1",
		Health: &npuctl.HealthView{
			State: "Healthy", AllocationAllowed: true,
			Devices: []npuctl.DeviceHealthView{
				{PCIAddress: "0000:18:00.0", State: "Healthy"},
				{PCIAddress: "0000:86:00.0", State: "Healthy"},
			},
		},
	}
	out := renderNodeDetail(d)
	if !strings.Contains(out, "HEALTH:     Healthy allocationAllowed=true\n") {
		t.Errorf("요약 줄이 산문이 아니다:\n%s", out)
	}
	if !strings.Contains(out, "STATE") {
		t.Errorf("장치별 행 표 헤더가 없다:\n%s", out)
	}
}

// TestRenderNodeDetail_광고표에헤더가있다 는 ADVERTISED 가 RESOURCE/QTY 헤더 있는
// 표로 나오는지 본다.
func TestRenderNodeDetail_광고표에헤더가있다(t *testing.T) {
	d := &npuctl.NodeDetail{
		Name:        "k8s-worker1",
		Allocatable: map[string]string{"nvidia.com/gpu": "2"},
	}
	out := renderNodeDetail(d)
	if !strings.Contains(out, "RESOURCE") || !strings.Contains(out, "QTY") {
		t.Errorf("광고 표 헤더가 없다:\n%s", out)
	}
}

// TestRenderNodeDetail_정책표에헤더가있다 는 POLICY 가 NAME/VENDOR/PHASE 헤더 있는
// 표로 나오는지 본다.
func TestRenderNodeDetail_정책표에헤더가있다(t *testing.T) {
	d := &npuctl.NodeDetail{
		Name:     "k8s-worker1",
		Policies: []npuctl.PolicyView{{Name: "demo-mig", Vendor: "nvidia", Phase: "Ready"}},
	}
	out := renderNodeDetail(d)
	if !strings.Contains(out, "PHASE") {
		t.Errorf("정책 표 헤더가 없다:\n%s", out)
	}
	if !strings.Contains(out, "demo-mig") || !strings.Contains(out, "Ready") {
		t.Errorf("정책 값이 안 보인다:\n%s", out)
	}
}

// TestRenderTable_폭은데이터에서잰다 는 헤더보다 긴 값이 있을 때 그 열 전체가
// 그 값 길이에 맞춰 넓어지는지 본다 — printTable 과 같은 원칙(고정폭 하드코딩 금지).
// 이 저장소는 이 규칙을 어겨 실제로 라이브 표가 어긋난 전례가 있다
// (TestPrintStatusText_ColumnsAlignWithLongestDeviceName 참조).
func TestRenderTable_폭은데이터에서잰다(t *testing.T) {
	out := renderTable(
		[]string{"PRODUCT", "PCI"},
		[][]string{
			{"tenstorrent/blackhole-p150", "0000:af:00.0"},
			{"nvidia/a30", "0000:18:00.0"},
		},
	)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("헤더 + 데이터 2행이어야 하는데 %d 줄: %q", len(lines), lines)
	}
	want := strings.Index(lines[0], "PCI")
	if want <= 0 {
		t.Fatalf("헤더에 PCI 열이 없다: %q", lines[0])
	}
	for _, ln := range lines[1:] {
		if got := strings.Index(ln, "0000:"); got != want {
			t.Errorf("PCI 열이 %d 에서 시작해야 하는데 %d 다: %q", want, got, ln)
		}
	}
}

// TestRenderTable_빈행이면헤더만 은 rows 가 비었을 때 헤더 줄만 나오는지 본다.
func TestRenderTable_빈행이면헤더만(t *testing.T) {
	out := renderTable([]string{"A", "B"}, nil)
	if strings.TrimRight(out, "\n") != "  A  B" {
		t.Errorf("빈 표 출력이 다르다: %q", out)
	}
}

// TestRenderTable_들여쓰기 는 헤더·행이 모두 접두어(들여쓰기 두 칸)를 갖는지 본다 —
// DEVICES: 같은 절 라벨 아래 붙는 표라 왼쪽에 여백이 있어야 절과 구분된다.
func TestRenderTable_들여쓰기(t *testing.T) {
	out := renderTable([]string{"A"}, [][]string{{"x"}})
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !strings.HasPrefix(ln, "  ") {
			t.Errorf("들여쓰기 없는 줄: %q", ln)
		}
	}
}
