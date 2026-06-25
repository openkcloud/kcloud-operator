/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"kcloud-operator/internal/upgrade"
)

// hasDoesNotExist는 required nodeAffinity 의 모든 term 에 (key, DoesNotExist) 요구사항이
// 존재하는지 확인한다.
func hasDoesNotExist(spec *corev1.PodSpec, key string) bool {
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil {
		return false
	}
	ns := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if ns == nil || len(ns.NodeSelectorTerms) == 0 {
		return false
	}
	for _, term := range ns.NodeSelectorTerms {
		found := false
		for _, req := range term.MatchExpressions {
			if req.Key == key && req.Operator == corev1.NodeSelectorOpDoesNotExist {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestApplyControlPlaneExclusion 는 control-plane/master 라벨 노드를 제외하는
// nodeAffinity 가 부착되고, 기존 affinity(driver-upgrade anti-affinity)가 보존되는지 검증한다.
func TestApplyControlPlaneExclusion(t *testing.T) {
	spec := &corev1.PodSpec{}
	// 기존 제약(driver-upgrade)이 먼저 적용된 상태를 모사.
	applyDriverUpgradeAntiAffinity(spec)
	applyControlPlaneExclusion(spec)

	if !hasDoesNotExist(spec, controlPlaneNodeLabel) {
		t.Errorf("control-plane 제외 요구사항이 없음")
	}
	if !hasDoesNotExist(spec, masterNodeLabel) {
		t.Errorf("master 제외 요구사항이 없음")
	}
	// 기존 driver-upgrade 제약이 보존되어야 한다(term 내 AND 누적).
	if !hasDoesNotExist(spec, upgrade.DriverUpgradingBlockingLabelKey) {
		t.Errorf("기존 driver-upgrade 제약이 손실됨")
	}
}
