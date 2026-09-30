// driverupgradestate_test.go: DriverUpgradeState CRD 필드 노출 시험
// 생성일: 2026-09-17

package v1alpha1

import (
	"os"
	"strings"
	"testing"
)

func TestDriverUpgradeStateCRDHasDriftFirstSeen(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/npu.ai_driverupgradestates.yaml")
	if err != nil {
		t.Fatalf("CRD 읽기 실패: %v", err)
	}
	if !strings.Contains(string(raw), "driftFirstSeen") {
		t.Error("CRD 에 driftFirstSeen 필드가 없다 — make manifests 재생성 필요")
	}
}
