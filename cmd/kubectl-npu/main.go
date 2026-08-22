// main.go: kubectl-npu plugin — NPU Operator CLI
// 상세: kubectl npu status[--json]/driver-version/upgrade/toggle/describe 명령을 제공.
//
//	조회·제어 로직은 pkg/npuctl(kcloud-operator 모듈, replace로 로컬 참조)에 위임하고,
//	이 파일은 CLI 파싱 + 사람용/JSON 출력 포맷팅만 담당한다(S5-3-① 관리 API 1단계).
//
// 생성일: 2026-03-25 | 수정일: 2026-08-24
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"kcloud-operator/pkg/npuctl"
)

// printTable은 열 폭을 데이터에서 재어 표를 낸다.
//
// 고정폭 서식(%-15s)은 값 하나만 넘쳐도 그 행부터 열이 밀려 표를 눈으로 훑을 수 없다.
// 실측만 봐도 노드 rngd-1.cluster.local(20자)과 장치 tenstorrent/blackhole-p150(26자)이
// 하드코딩한 폭을 넘겼다. 클러스터마다 이름 길이가 다르므로 폭은 잴 수밖에 없다.
func printTable(header []string, rows [][]string) {
	width := make([]int, len(header))
	for i, h := range header {
		width[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(width) {
				width[i] = maxInt(width[i], len(c))
			}
		}
	}
	line := func(cells []string) {
		var b strings.Builder
		for i, c := range cells {
			if i == len(cells)-1 {
				b.WriteString(c)
				break
			}
			fmt.Fprintf(&b, "%-*s  ", width[i], c)
		}
		fmt.Println(strings.TrimRight(b.String(), " "))
	}
	line(header)
	for _, r := range rows {
		line(r)
	}
}

// renderTable 은 printTable 과 같은 정렬 원칙(폭을 헤더·데이터 중 더 긴 쪽에서 잰다,
// 고정폭 하드코딩 금지)을 쓰되 표준출력에 바로 찍지 않고 문자열로 돌려준다.
// describe 명령의 각 절은 문자열을 조립해 한 번에 반환해야 해서 printTable(표준출력
// 직접 기록)을 그대로 못 쓴다 — printTable 은 다른 네 명령이 공유하므로 건드리지 않는다.
// 매 줄 앞에 2칸을 들여써서 절 라벨(DEVICES: 등) 아래 붙는 하위 표임을 나타낸다.
func renderTable(header []string, rows [][]string) string {
	width := make([]int, len(header))
	for i, h := range header {
		width[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(width) {
				width[i] = maxInt(width[i], len(c))
			}
		}
	}
	var b strings.Builder
	line := func(cells []string) {
		var lb strings.Builder
		lb.WriteString("  ")
		for i, c := range cells {
			if i == len(cells)-1 {
				lb.WriteString(c)
				break
			}
			fmt.Fprintf(&lb, "%-*s  ", width[i], c)
		}
		b.WriteString(strings.TrimRight(lb.String(), " "))
		b.WriteString("\n")
	}
	line(header)
	for _, r := range rows {
		line(r)
	}
	return b.String()
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "status":
		cmdStatus(os.Args[2:])
	case "driver-version":
		cmdDriverVersion()
	case "upgrade":
		cmdUpgrade(os.Args[2:])
	case "toggle":
		cmdToggle(os.Args[2:])
	case "describe":
		cmdDescribe(os.Args[2:])
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`kubectl-npu: NPU Operator CLI Plugin

Usage:
  kubectl npu status                              노드×벤더 집약 상태(텍스트) 출력
  kubectl npu status --json                       위와 동일한 내용을 JSON으로 출력(관리툴 소비용)
  kubectl npu driver-version                       노드별 설치된 드라이버 버전 표
  kubectl npu upgrade <vendor> --version VER [--model M] [--force] [--auto]
                                                    DriverInstallPolicy.spec.driver.version patch
  kubectl npu toggle <vendor> --enabled true|false NPUClusterPolicy.spec.<vendor>.enabled patch
  kubectl npu describe node [<노드>]                노드 하나(또는 전체)의 장치·광고량·health·evidence·정책
  kubectl npu describe node [<노드>] --json         위와 동일한 내용을 JSON 으로

Vendors: nvidia, furiosa, rngd(furiosa RNGD 서브벤더), rebellions, tenstorrent

Examples:
  kubectl npu status
  kubectl npu status --json
  kubectl npu driver-version
  kubectl npu upgrade nvidia --version 580.126.10 --auto
  kubectl npu upgrade furiosa --model rngd --version 2026.2.0 --force
  kubectl npu toggle rngd --enabled true
  kubectl npu describe node k8s-worker1
  kubectl npu describe node                        (전체 노드)`)
}

func newClientOrExit() *npuctl.Client {
	c, err := npuctl.NewClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return c
}

func cmdStatus(args []string) {
	asJSON := false
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		}
	}

	c := newClientOrExit()
	status, err := c.CollectStatus(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error collecting status: %v\n", err)
		os.Exit(1)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(status); err != nil {
			fmt.Fprintf(os.Stderr, "Error encoding JSON: %v\n", err)
			os.Exit(1)
		}
		return
	}

	printStatusText(status)
}

// printStatusText는 기존 kubectl-npu status의 출력 형식(DriverInstallPolicies/NodeDeviceReports/
// NPUClusterPolicy 3단 텍스트 표)을 npuctl.ClusterStatus로부터 재구성하여 하위호환을 유지한다.
func printStatusText(status *npuctl.ClusterStatus) {
	fmt.Println("=== DriverInstallPolicies ===")
	var dipRows [][]string
	for _, v := range status.Vendors {
		for _, dip := range v.DriverInstallPolicies {
			dipRows = append(dipRows, []string{dip.Name, dip.Vendor, dip.Model, dip.DesiredVersion})
		}
	}
	printTable([]string{"NAME", "VENDOR", "MODEL", "VERSION"}, dipRows)

	fmt.Println("\n=== NodeDeviceReports ===")
	var ndrRows [][]string
	for _, n := range status.Nodes {
		// note는 배제 사유다. operator 가 붙인 라벨을 그대로 옮긴 값이라(NodeStatus.Excluded)
		// 여기서 다시 판정하지 않는다 — 배제 아닌 노드는 빈 문자열로 마지막 열이 트림된다.
		// 사유가 비면 빈 괄호(`제외됨()`)를 내지 않는다.
		note := ""
		if n.Excluded {
			note = "제외됨"
			if n.ExcludedReason != "" {
				note = fmt.Sprintf("제외됨(%s)", n.ExcludedReason)
			}
		}
		// 가속기 없는 배제 노드(GPU 없는 control-plane 이 일반적 형상)도 행을 낸다 — 배제
		// 표시를 장치 루프 안에만 두면 status.devices 가 비어 행이 0개가 되고 라벨이 붙어
		// 있어도 화면에서 사라진다. 배제 아닌 장치 0개 노드는 기존대로 행을 내지 않는다.
		if len(n.Devices) == 0 && n.Excluded {
			ndrRows = append(ndrRows, []string{n.Name, "—", "—", "—", "—", "—", note})
		}
		for _, d := range n.Devices {
			ndrRows = append(ndrRows, []string{
				n.Name, d.Product, d.PCIeAddress,
				strconv.FormatInt(int64(d.Count), 10), strconv.FormatBool(d.DriverLoaded), d.DriverVersion,
				note,
			})
		}
	}
	printTable([]string{"NODE", "DEVICE", "PCI", "COUNT", "LOADED", "VERSION", "NOTE"}, ndrRows)

	fmt.Println("\n=== NPUClusterPolicy ===")
	if len(status.ClusterPolicies) == 0 {
		fmt.Println("  (no NPUClusterPolicy found)")
	}
	for _, cp := range status.ClusterPolicies {
		if cp.Ready == "" {
			fmt.Printf("  %s/%s: no conditions\n", cp.Namespace, cp.Name)
			continue
		}
		fmt.Printf("  %s/%s: Ready=%s (%s)\n", cp.Namespace, cp.Name, cp.Ready, cp.ReadyReason)
	}

	fmt.Println("\n=== DriverUpgradeState ===")
	var upRows [][]string
	for _, n := range status.Nodes {
		for _, u := range n.Upgrades {
			upRows = append(upRows, []string{n.Name, u.Vendor, u.State, u.CurrentVersion, u.DesiredVersion})
		}
	}
	printTable([]string{"NODE", "VENDOR", "STATE", "CURRENT", "DESIRED"}, upRows)
}

func cmdDriverVersion() {
	c := newClientOrExit()
	status, err := c.CollectStatus(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	var rows [][]string
	for _, n := range status.Nodes {
		for _, d := range n.Devices {
			rows = append(rows, []string{
				n.Name, d.Vendor, d.Model, d.DriverVersion, strconv.FormatBool(d.DriverLoaded),
			})
		}
	}
	printTable([]string{"NODE", "VENDOR", "MODEL", "DRIVER_VER", "LOADED"}, rows)
}

func cmdUpgrade(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kubectl npu upgrade <vendor> --version <ver> [--model <model>] [--force] [--auto]")
		os.Exit(1)
	}

	vendor := args[0]
	opts := npuctl.UpgradeOptions{}
	version := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--version":
			if i+1 < len(args) {
				version = args[i+1]
				i++
			}
		case "--model":
			if i+1 < len(args) {
				opts.Model = args[i+1]
				i++
			}
		case "--force":
			opts.Force = true
		case "--auto":
			opts.Auto = true
		}
	}

	if version == "" {
		fmt.Fprintln(os.Stderr, "Error: --version is required")
		os.Exit(1)
	}

	c := newClientOrExit()
	result, err := c.UpgradeVendor(context.Background(), vendor, version, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Vendor:          %s\n", result.Vendor)
	fmt.Printf("Policy:          %s\n", result.PolicyName)
	fmt.Printf("Previous Version: %s\n", result.PreviousVersion)
	fmt.Printf("New Version:     %s\n", result.NewVersion)

	if result.NoChange {
		fmt.Println("Version already set. No change needed.")
		return
	}

	fmt.Printf("✓ DriverInstallPolicy %q patched: %s → %s\n", result.PolicyName, result.PreviousVersion, result.NewVersion)
	if result.ForceAnnotationSet {
		fmt.Println("✓ Force upgrade annotation set on NPUClusterPolicy")
	}

	fmt.Println()
	fmt.Println("Monitor progress:")
	fmt.Println("  kubectl get pods -n kube-system | grep npu-op-installer")
	fmt.Println("  kubectl get events --field-selector reason=UpgradeStarted")
	fmt.Println("  kubectl npu driver-version")
}

func cmdToggle(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kubectl npu toggle <vendor> --enabled <true|false>")
		os.Exit(1)
	}

	vendor := args[0]
	enabledStr := ""
	for i := 1; i < len(args); i++ {
		if args[i] == "--enabled" && i+1 < len(args) {
			enabledStr = args[i+1]
			i++
		}
	}
	if enabledStr != "true" && enabledStr != "false" {
		fmt.Fprintln(os.Stderr, "Error: --enabled must be 'true' or 'false'")
		os.Exit(1)
	}

	c := newClientOrExit()
	result, err := c.SetVendorEnabled(context.Background(), vendor, enabledStr == "true")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if result.NoChange {
		fmt.Printf("%s.enabled is already %v. No change needed.\n", result.Vendor, result.NewEnabled)
		return
	}
	fmt.Printf("✓ NPUClusterPolicy.spec.%s.enabled patched: %v → %v\n", result.Vendor, result.PreviousEnabled, result.NewEnabled)
}

// parseDescribeArgs 는 "node [<이름>] [--json]" 을 파싱한다. 이름을 생략하면
// name=="" 을 돌려주고 ok=true 다 — 호출부(cmdDescribe)가 이걸 "전체 노드 조회" 로
// 해석한다. args[0] 이 "node" 가 아니거나 아예 없으면 ok=false.
func parseDescribeArgs(args []string) (name string, asJSON bool, ok bool) {
	if len(args) < 1 || args[0] != "node" {
		return "", false, false
	}
	rest := args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "--") {
		name = rest[0]
		rest = rest[1:]
	}
	for _, a := range rest {
		if a == "--json" {
			asJSON = true
		}
	}
	return name, asJSON, true
}

func cmdDescribe(args []string) {
	name, asJSON, ok := parseDescribeArgs(args)
	if !ok {
		fmt.Fprintln(os.Stderr, "Usage: kubectl npu describe node [<노드>] [--json]")
		os.Exit(1)
	}

	c := newClientOrExit()
	ctx := context.Background()

	if name != "" {
		detail, err := c.CollectNodeDetail(ctx, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		printNodeDetail(detail, asJSON)
		return
	}

	names, err := c.ListNodeNames(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// details 는 조회 실패 노드를 걸러낸 목록이다 — ListNodeNames·CollectNodeDetail
	// 사이의 경합(노드 삭제 등)으로 일부가 실패해도 나머지는 잃지 않는다. JSON·텍스트
	// 두 출력 모두 이 걸러진 목록을 쓰므로 아래 renderNodeDetails 가 빈 줄을 셀 때
	// 실패한 노드의 인덱스를 신경 쓸 필요가 없다(실패 노드는 애초에 목록에 없다).
	details := make([]*npuctl.NodeDetail, 0, len(names))
	for _, n := range names {
		detail, err := c.CollectNodeDetail(ctx, n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error (%s): %v\n", n, err)
			continue
		}
		details = append(details, detail)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(details); err != nil {
			fmt.Fprintf(os.Stderr, "Error encoding JSON: %v\n", err)
			os.Exit(1)
		}
		return
	}

	fmt.Print(renderNodeDetails(details))
}

// renderNodeDetails 는 여러 노드 상세를 이어 붙인다 — 블록 사이 빈 줄 하나, 처음과 끝에는
// 없음. details 는 이미 조회 실패 노드를 걸러낸 목록이라고 가정한다.
func renderNodeDetails(details []*npuctl.NodeDetail) string {
	var sb strings.Builder
	for i, d := range details {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(renderNodeDetail(d))
	}
	return sb.String()
}

// printNodeDetail 은 단일 노드 조회 결과를 텍스트 또는 JSON 으로 낸다.
func printNodeDetail(detail *npuctl.NodeDetail, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(detail); err != nil {
			fmt.Fprintf(os.Stderr, "Error encoding JSON: %v\n", err)
			os.Exit(1)
		}
		return
	}
	fmt.Print(renderNodeDetail(detail))
}

// isAcceleratorResource 는 allocatable 키가 가속기 리소스인지를 이름 목록이 아니라
// 규칙으로 가른다 — internal/apiserver/ui/index.html 의 같은 규칙과 짝을 맞춘다.
// 벤더 이름을 박아두면 새 벤더의 광고량이 조용히 빠진다.
func isAcceleratorResource(key string) bool {
	if !strings.Contains(key, "/") {
		return false
	}
	for _, prefix := range []string{"cpu", "memory", "pods", "ephemeral-storage", "hugepages-", "attachable-volumes-"} {
		if strings.HasPrefix(key, prefix) {
			return false
		}
	}
	return true
}

// renderNodeDetail 은 노드 하나의 상세를 사람이 읽는 텍스트로 낸다. 다른 컨트롤러가
// 아직 안 적어 없는 값(health·evidence·policy 등)은 추측해 채우지 않고 "—" 로 낸다.
// 장치 표는 printTable(다른 명령이 공유)을 건드리지 않고 이 함수 안에서 직접 구성한다.
func renderNodeDetail(d *npuctl.NodeDetail) string {
	var b strings.Builder

	node := d.Name
	if d.Excluded {
		if d.ExcludedReason != "" {
			node += fmt.Sprintf("  제외됨(%s)", d.ExcludedReason)
		} else {
			node += "  제외됨"
		}
	}
	fmt.Fprintf(&b, "NODE:       %s\n", node)

	if len(d.Devices) == 0 {
		fmt.Fprintf(&b, "DEVICES:    —\n")
	} else {
		fmt.Fprintf(&b, "DEVICES:\n")
		rows := make([][]string, len(d.Devices))
		for i, dev := range d.Devices {
			rows[i] = []string{dev.Product, dev.PCIeAddress, strconv.Itoa(int(dev.Count)),
				strconv.FormatBool(dev.DriverLoaded), dev.DriverVersion}
		}
		b.WriteString(renderTable([]string{"PRODUCT", "PCI", "COUNT", "LOADED", "DRIVER"}, rows))
	}

	var acc []string
	for k := range d.Allocatable {
		if isAcceleratorResource(k) {
			acc = append(acc, k)
		}
	}
	sort.Strings(acc)
	if len(acc) == 0 {
		fmt.Fprintf(&b, "ADVERTISED: —\n")
	} else {
		fmt.Fprintf(&b, "ADVERTISED:\n")
		rows := make([][]string, len(acc))
		for i, k := range acc {
			rows[i] = []string{k, d.Allocatable[k]}
		}
		b.WriteString(renderTable([]string{"RESOURCE", "QTY"}, rows))
	}

	if d.Health == nil {
		fmt.Fprintf(&b, "HEALTH:     —\n")
	} else {
		// NODE 줄과 같은 규칙 — Reason 이 비면 빈 괄호를 내지 않는다.
		health := d.Health.State
		if d.Health.Reason != "" {
			health += fmt.Sprintf(" (%s)", d.Health.Reason)
		}
		fmt.Fprintf(&b, "HEALTH:     %s allocationAllowed=%v\n", health, d.Health.AllocationAllowed)
		if len(d.Health.Devices) > 0 {
			rows := make([][]string, len(d.Health.Devices))
			for i, dh := range d.Health.Devices {
				rows[i] = []string{dh.PCIAddress, dh.State, dh.Reason}
			}
			b.WriteString(renderTable([]string{"PCI", "STATE", "REASON"}, rows))
		}
	}

	if len(d.Evidence) == 0 {
		fmt.Fprintf(&b, "EVIDENCE:   —\n")
	} else {
		rows := make([][]string, len(d.Evidence))
		noIdentifier := false
		for i, ev := range d.Evidence {
			vendor, level := ev.Vendor, ev.Level
			// Vendor·Level 이 둘 다 빈 조각(라이브에서 실재)을 표 칸에 그대로 두면
			// 만료일만 남는다. "식별자 없음" 문자열은 바이트 수(16)가 화면 폭(11)보다
			// 커서 renderTable 이 표 폭을 재는 len() 과 어긋나 LEVEL/EXPIRES 열이
			// 밀린다(k8s-master·k8s-worker3 실측) — 표 칸에는 다른 빈 값과 같은
			// "—" 를 쓰고, 식별자가 없다는 사실은 라벨 줄에 적는다.
			if vendor == "" && level == "" {
				noIdentifier = true
				vendor, level = "—", "—"
			}
			rows[i] = []string{vendor, level, ev.ExpiresAt}
		}
		if noIdentifier {
			fmt.Fprintf(&b, "EVIDENCE:   식별자 없음\n")
		} else {
			fmt.Fprintf(&b, "EVIDENCE:\n")
		}
		b.WriteString(renderTable([]string{"VENDOR", "LEVEL", "EXPIRES"}, rows))
	}

	if len(d.Policies) == 0 {
		fmt.Fprintf(&b, "POLICY:     —\n")
	} else {
		fmt.Fprintf(&b, "POLICY:\n")
		rows := make([][]string, len(d.Policies))
		for i, p := range d.Policies {
			rows[i] = []string{p.Name, p.Vendor, p.Phase}
		}
		b.WriteString(renderTable([]string{"NAME", "VENDOR", "PHASE"}, rows))
	}

	return b.String()
}

// maxInt 는 두 정수 중 큰 값. 표 열 폭을 데이터에서 잴 때 쓴다.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
