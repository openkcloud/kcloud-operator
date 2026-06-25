// main.go: kubectl-npu plugin — NPU Operator CLI
// 상세: kubectl npu status[--json]/driver-version/upgrade/toggle 명령을 제공.
//
//	조회·제어 로직은 pkg/npuctl(kcloud-operator 모듈, replace로 로컬 참조)에 위임하고,
//	이 파일은 CLI 파싱 + 사람용/JSON 출력 포맷팅만 담당한다(S5-3-① 관리 API 1단계).
//
// 생성일: 2026-03-25 | 수정일: 2026-07-16
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"kcloud-operator/pkg/npuctl"
)

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

Vendors: nvidia, furiosa, rngd(furiosa RNGD 서브벤더), rebellions, tenstorrent

Examples:
  kubectl npu status
  kubectl npu status --json
  kubectl npu driver-version
  kubectl npu upgrade nvidia --version 580.126.10 --auto
  kubectl npu upgrade furiosa --model rngd --version 2026.2.0 --force
  kubectl npu toggle rngd --enabled true`)
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
	fmt.Printf("%-20s %-12s %-10s %-15s\n", "NAME", "VENDOR", "MODEL", "VERSION")
	for _, v := range status.Vendors {
		for _, dip := range v.DriverInstallPolicies {
			fmt.Printf("%-20s %-12s %-10s %-15s\n", dip.Name, dip.Vendor, dip.Model, dip.DesiredVersion)
		}
	}

	fmt.Println("\n=== NodeDeviceReports ===")
	fmt.Printf("%-15s %-12s %-10s %-6s %-8s %-15s\n", "NODE", "VENDOR", "MODEL", "COUNT", "LOADED", "VERSION")
	for _, n := range status.Nodes {
		for _, d := range n.Devices {
			fmt.Printf("%-15s %-12s %-10s %-6d %-8t %-15s\n", n.Name, d.Vendor, d.Model, d.Count, d.DriverLoaded, d.DriverVersion)
		}
	}

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
	fmt.Printf("%-15s %-12s %-20s %-15s %-15s\n", "NODE", "VENDOR", "STATE", "CURRENT", "DESIRED")
	for _, n := range status.Nodes {
		for _, u := range n.Upgrades {
			fmt.Printf("%-15s %-12s %-20s %-15s %-15s\n", n.Name, u.Vendor, u.State, u.CurrentVersion, u.DesiredVersion)
		}
	}
}

func cmdDriverVersion() {
	c := newClientOrExit()
	status, err := c.CollectStatus(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%-15s %-12s %-10s %-15s %-8s\n", "NODE", "VENDOR", "MODEL", "DRIVER_VER", "LOADED")
	for _, n := range status.Nodes {
		for _, d := range n.Devices {
			fmt.Printf("%-15s %-12s %-10s %-15s %-8t\n", n.Name, d.Vendor, d.Model, d.DriverVersion, d.DriverLoaded)
		}
	}
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
