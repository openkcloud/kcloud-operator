// main_test.go: kubectl-npu 사람용 텍스트 출력 포맷 테스트
// 상세: 같은 노드에 붙은 장치 여러 대가 표에서 서로 구분되는지 본다(라이브에서 동일 행 중복 관측).
// 생성일: 2026-08-11
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
