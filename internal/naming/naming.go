// ============================================================
// naming.go: 드라이버 DaemonSet 이름 생성 규칙 중앙화
// 상세: driver_daemonset_controller(생성)와 upgrade/state_machine(조회)이
//       동일한 DS 이름을 사용하도록 단일 헬퍼로 통일한다. 불일치 시
//       업그레이드 상태머신이 드라이버 DS를 찾지 못한다.
// 생성일: 2026-06-02 | 수정일: 2026-06-02
// ============================================================

// Package naming centralizes Kubernetes resource naming rules shared across
// the controller and upgrade packages to avoid divergence.
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// 벤더 문자열 상수 — 매핑 switch 에서 반복 사용(goconst 회피).
const (
	vendorNvidia  = "nvidia"
	vendorFuriosa = "furiosa"
)

// KubeSystemNamespace 는 3rd-party device-plugin 이 상주하는 시스템 네임스페이스(#16 예외).
// operator 부속이 kcloud 로 이동해도 device-plugin 은 여기 고정. 문자열 중복(goconst) 방지 겸용.
const KubeSystemNamespace = "kube-system"

// operatorNamespaceDefault 는 OPERATOR_NAMESPACE env 미설정 시 기본값.
// 기존 배치(kube-system)와 동일하게 두어 env 미설정 시 회귀 0(무해 no-op)을 보장한다.
const operatorNamespaceDefault = KubeSystemNamespace

// OperatorNamespace 는 operator 가 관리하는 워크로드(driver DaemonSet / install Job /
// toolkit DaemonSet)를 생성·조회할 네임스페이스를 반환한다. helm 이 Downward API 로
// 주입하는 `OPERATOR_NAMESPACE`(=release namespace) 를 사용하며, 미설정 시 kube-system.
// ⚠️ device-plugin(3rd party)은 이 값이 아니라 kube-system 에 고정 유지한다(#16 예외).
func OperatorNamespace() string {
	if ns := os.Getenv("OPERATOR_NAMESPACE"); ns != "" {
		return ns
	}
	return operatorNamespaceDefault
}

// DriverDSName returns the driver DaemonSet name for a given vendor/model.
//
// Mapping:
//   - nvidia      → "kcloud-nvidia-driver"     (model 무시)
//   - furiosa     → "kcloud-furiosa-<model>-driver" (model 비면 "kcloud-furiosa-driver")
//   - default     → "kcloud-<vendor>[-<model>]-driver"
//
// 빈 model 안전성: model 이 비어 있으면 이중 하이픈("--") 없이 model 세그먼트를 생략한다.
func DriverDSName(vendor, model string) string {
	v := strings.ToLower(vendor)
	m := strings.ToLower(model)
	switch v {
	case vendorNvidia:
		return "kcloud-nvidia-driver"
	case vendorFuriosa:
		if m == "" {
			return "kcloud-furiosa-driver"
		}
		return "kcloud-furiosa-" + m + "-driver"
	default:
		if m == "" {
			return "kcloud-" + v + "-driver"
		}
		return "kcloud-" + v + "-" + m + "-driver"
	}
}

// InstallJobName returns the driver install Job name for a given vendor/model/node.
//
// WP-C-1(Mode=job): 순간 install Job 의 이름 규약. DriverDSName 과 대칭이되 노드별로
// 분리되며(노드 고정 Job), 결정적 이름이 곧 동시성 lease 역할을 한다 — 동일
// (vendor,model,node) 에는 항상 같은 이름이 나오므로 중복 Job 생성이 구조적으로 불가능하다
// (Get-before-Create + AlreadyExists 방어).
//
// 형식: "kcloud-<vendor>[-<model>]-install-<8hex>"
//   - <8hex> = sha256(nodeName) 앞 8 hex. 노드명이 DNS-1123 label(63자) 제약이나
//     허용문자를 위반해도 안전한 접미사를 보장한다.
//   - vendor/model 세그먼트는 DriverDSName 과 동일 규칙(빈 model 이면 세그먼트 생략).
func InstallJobName(vendor, model, nodeName string) string {
	v := strings.ToLower(vendor)
	m := strings.ToLower(model)
	sum := sha256.Sum256([]byte(nodeName))
	short := hex.EncodeToString(sum[:])[:8]

	var prefix string
	switch v {
	case vendorNvidia:
		prefix = "kcloud-nvidia"
	case vendorFuriosa:
		if m == "" {
			prefix = "kcloud-furiosa"
		} else {
			prefix = "kcloud-furiosa-" + m
		}
	default:
		if m == "" {
			prefix = "kcloud-" + v
		} else {
			prefix = "kcloud-" + v + "-" + m
		}
	}
	return prefix + "-install-" + short
}

// RebootJobName 은 노드 재부팅 Job 의 결정론적 이름이다(S2-5). node 해시로 중복 생성을
// 방지(de-facto lease) — 벤더 무관(재부팅은 host 커널 문제라 노드 단위).
func RebootJobName(nodeName string) string {
	sum := sha256.Sum256([]byte(nodeName))
	return "kcloud-node-reboot-" + hex.EncodeToString(sum[:])[:8]
}

// ToolkitDSName returns the container-toolkit DaemonSet name for a given vendor/model.
// DriverDSName 과 동일한 규칙에 "-toolkit" 접미사만 다르게 하여, driver / toolkit DS 를
// 노드에서 짝으로 식별할 수 있게 한다.
//
// Mapping:
//   - nvidia   → "kcloud-nvidia-toolkit" (model 무시)
//   - furiosa  → "kcloud-furiosa-<model>-toolkit" (model 비면 "kcloud-furiosa-toolkit")
//   - default  → "kcloud-<vendor>[-<model>]-toolkit"
func ToolkitDSName(vendor, model string) string {
	v := strings.ToLower(vendor)
	m := strings.ToLower(model)
	switch v {
	case vendorNvidia:
		return "kcloud-nvidia-toolkit"
	case vendorFuriosa:
		if m == "" {
			return "kcloud-furiosa-toolkit"
		}
		return "kcloud-furiosa-" + m + "-toolkit"
	default:
		if m == "" {
			return "kcloud-" + v + "-toolkit"
		}
		return "kcloud-" + v + "-" + m + "-toolkit"
	}
}
