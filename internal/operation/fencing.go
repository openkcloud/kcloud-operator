// ============================================================
// fencing.go: 세대(epoch) 발급 (R&D base v0.1 §7.8)
// 상세: 노드 잠금을 새로 잡을 때마다 세대가 오른다. 낡은 프로세스의 status 기록은
//
//	operation_controller.go 의 writeStatus 가 서버 세대와 방향 비교해 폐기한다 —
//	개별 저널 항목을 세대로 걸러내는 도우미는 두지 않는다. 크래시 복구 판정이 세대를
//	가로질러 이력을 봐야 하기 때문이다(recovery.go 의 HasStep).
//
// 생성일: 2026-08-01
// ============================================================
package operation

// NextEpoch 는 다음 세대다.
func NextEpoch(cur int64) int64 { return cur + 1 }
