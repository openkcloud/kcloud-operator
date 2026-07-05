// ============================================================
// observe.go: operator-driven MIG 관측 — privileged nsenter Job + fail-closed parse (spec §16)
// 상세: detector 가 host nvidia-smi 실행 불가라, operator 가 mig-tool Job(nsenter)으로 MIG
//
//	mode/geometry/lgip 를 per-PCI 관측한다. 관측 결과는 Backend.WithObservations 로 in-memory
//	주입(NDR 미patch). parse 규칙은 detector 와 동일 fail-closed(§15.3).
//
// 생성일: 2026-07-27
// ============================================================
package nvidia

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MIG mode/geometry 문자열 상수(fail-closed 판정에 반복 등장 — goconst).
const (
	modeEnabled  = "Enabled"
	modeDisabled = "Disabled"
	modeNA       = "NA"
	modeUnknown  = "Unknown"
	geomDisabled = "disabled"
)

// Observation 은 단일 GPU(PCI)의 operator-관측 MIG 상태다(fail-closed, spec §16.2/§15.3).
type Observation struct {
	PCI, ModeCurrent, ModePending, Geometry, LgipOutput, Err string
}

// Observer 는 노드의 per-PCI MIG 상태를 관측하는 seam 이다(reconciler 가 fresh 관측을 Backend 에
// 주입할 때 사용, spec §16.3). 실 구현은 *MigObserver(Job), 테스트는 fake 로 대체한다.
type Observer interface {
	Observe(ctx context.Context, nodeName string, pcis []string) ([]Observation, error)
}

var _ Observer = (*MigObserver)(nil)

// migGiLineRe 는 `mig -lgi` 출력의 GI 행에서 "1g.6gb" 프로파일 토큰을 추출한다("+me" 변형 포함).
var migGiLineRe = regexp.MustCompile(`MIG\s+(\d+g\.\d+gb)`)

// normalizeMigMode 는 nvidia-smi mig.mode.current/pending CSV 필드값을 정규화한다.
// 인식 못하는 값은 전부 "Unknown"(fail-closed) — 조용히 "Disabled" 로 오판하지 않는다.
func normalizeMigMode(s string) string {
	switch strings.TrimSpace(s) {
	case modeEnabled:
		return modeEnabled
	case modeDisabled:
		return modeDisabled
	case "N/A", "[N/A]":
		return modeNA
	default:
		return modeUnknown
	}
}

// parseObservation 은 fail-closed 로 관측을 판정한다(§15.3): mode csv 파싱 실패, current/pending
// Unknown, 또는 Enabled 인데 -lgi 를 파싱 못하면 전부 Unknown+Err 를 반환하고 절대 조용히
// "disabled" 로 보고하지 않는다(detector parseMigObservation 포팅).
func parseObservation(pci, modeCsv, lgi, lgip string) Observation {
	f := strings.Split(strings.TrimSpace(modeCsv), ",")
	if len(f) != 2 || strings.TrimSpace(f[0]) == "" {
		return Observation{PCI: pci, ModeCurrent: modeUnknown, ModePending: modeUnknown, Err: "unparseable mode csv"}
	}
	cur, pend := normalizeMigMode(f[0]), normalizeMigMode(f[1])
	if cur == modeUnknown {
		return Observation{PCI: pci, ModeCurrent: modeUnknown, ModePending: pend, LgipOutput: lgip, Err: "unrecognized mig.mode.current"}
	}
	if pend == modeUnknown {
		return Observation{PCI: pci, ModeCurrent: cur, ModePending: modeUnknown, LgipOutput: lgip, Err: "unrecognized mig.mode.pending"}
	}
	obs := Observation{PCI: pci, ModeCurrent: cur, ModePending: pend, LgipOutput: lgip}
	switch cur {
	case modeDisabled:
		obs.Geometry = geomDisabled
	case modeNA:
		// MIG 미지원 GPU — 분할될 여지가 없으므로 trivially "disabled".
		obs.Geometry = geomDisabled
	case modeEnabled:
		g, err := summarizeMigGeometryStrict(lgi)
		if err != nil {
			return Observation{PCI: pci, ModeCurrent: modeUnknown, ModePending: pend, LgipOutput: lgip, Err: "enabled but lgi unparseable: " + err.Error()}
		}
		obs.Geometry = g
	}
	return obs
}

// summarizeMigGeometryStrict 는 `mig -lgi` 원문을 요약한다. current=Enabled 인데 파싱 불가면
// error 를 반환한다(fail-closed) — Enabled 상태에서 조용히 "disabled" 로 보고하지 않는다.
// 모델 B(§17.1): Enabled + "No GPU instances found"(GI 없음) → "" (enabled-no-GI, 안전 baseline).
func summarizeMigGeometryStrict(lgi string) (string, error) {
	if strings.Contains(lgi, "No MIG-enabled devices") {
		return geomDisabled, nil
	}
	if strings.Contains(lgi, "No GPU instances found") {
		return "", nil // MIG enabled 이나 GI 없음 — 안전 baseline(모델 B).
	}
	if strings.TrimSpace(lgi) == "" {
		return "", fmt.Errorf("empty lgi")
	}
	counts := map[string]int{}
	for _, line := range strings.Split(lgi, "\n") {
		if m := migGiLineRe.FindStringSubmatch(line); m != nil {
			counts[m[1]]++
		}
	}
	if len(counts) == 0 {
		return "", fmt.Errorf("no parseable GI in non-empty lgi")
	}
	parts := make([]string, 0, len(counts))
	for p, n := range counts {
		parts = append(parts, fmt.Sprintf("%s x%d", p, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", "), nil
}

const (
	observePollInterval = 2 * time.Second
	observeTimeout      = 2 * time.Minute
	observeCreateWait   = 30 * time.Second
)

// MigObserver 는 privileged nsenter Job 으로 대상 노드의 per-PCI MIG 상태를 관측한다(spec §16.2).
type MigObserver struct {
	c         client.Client
	namespace string
	image     string // mig-tool 이미지(ACPP_MIG_JOB_IMAGE) — nsenter 포함
}

func NewMigObserver(c client.Client, namespace, image string) *MigObserver {
	return &MigObserver{c: c, namespace: namespace, image: image}
}

// Observe 는 nodeName 의 각 pci 에 대해 mode/-lgi/-lgip 를 privileged Job 으로 조회하고
// fail-closed 로 parse 한 Observation 을 pci 당 하나씩 반환한다(§16.2). Job 실패·pod 소실·빈
// 메시지·section 누락은 조용히 드롭하지 않고 Err 를 채운 Observation 으로 표면화한다(apply 차단용).
func (o *MigObserver) Observe(ctx context.Context, nodeName string, pcis []string) ([]Observation, error) {
	if len(pcis) == 0 {
		return nil, nil
	}
	name := "acpp-mig-observe-" + shortHash(nodeName)
	// 이전 Job(TTL 잔존/실패)을 먼저 정리해 stale 성공을 재사용하지 않는다.
	o.deleteJob(ctx, name)
	if err := o.createTolerant(ctx, renderObserveJob(name, nodeName, pcis, o.image, o.namespace)); err != nil {
		return failClosed(pcis, "observe job create: "+err.Error()), err
	}
	if err := o.waitDone(ctx, name); err != nil {
		return failClosed(pcis, "observe job: "+err.Error()), err
	}
	msg, err := o.terminationMessage(ctx, name)
	if err != nil {
		return failClosed(pcis, "observe message: "+err.Error()), err
	}
	obs := parseDelimitedMessage(msg, pcis)
	o.deleteJob(ctx, name) // best-effort 즉시 정리(실패해도 TTL 이 회수).
	return obs, nil
}

// deleteJob 은 Job(+pods, background propagation)을 best-effort 삭제한다(NotFound 무시).
func (o *MigObserver) deleteJob(ctx context.Context, name string) {
	bg := metav1.DeletePropagationBackground
	_ = o.c.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: o.namespace}},
		&client.DeleteOptions{PropagationPolicy: &bg})
}

// createTolerant 는 Job 을 생성하되, 직전 삭제의 background 정리가 아직 안 끝나 AlreadyExists 면
// ctx-aware 로 잠시 재시도한다.
func (o *MigObserver) createTolerant(ctx context.Context, job *batchv1.Job) error {
	deadline := time.NewTimer(observeCreateWait)
	defer deadline.Stop()
	ticker := time.NewTicker(observePollInterval)
	defer ticker.Stop()
	for {
		err := o.c.Create(ctx, job)
		if err == nil || !apierrors.IsAlreadyExists(err) {
			return err
		}
		o.deleteJob(ctx, job.Name)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("observe job %q still exists after %s", job.Name, observeCreateWait)
		case <-ticker.C:
		}
	}
}

// waitDone 은 Job 완료(Succeeded/Failed)를 ctx-aware 폴링으로 대기한다(migjob.go wait 패턴 재사용).
func (o *MigObserver) waitDone(ctx context.Context, name string) error {
	ticker := time.NewTicker(observePollInterval)
	defer ticker.Stop()
	deadline := time.NewTimer(observeTimeout)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("observe job %q timed out after %s", name, observeTimeout)
		case <-ticker.C:
			var job batchv1.Job
			if err := o.c.Get(ctx, types.NamespacedName{Name: name, Namespace: o.namespace}, &job); err != nil {
				return err
			}
			if job.Status.Succeeded > 0 {
				return nil
			}
			if job.Status.Failed > 0 {
				return fmt.Errorf("observe job %q failed", name)
			}
		}
	}
}

// terminationMessage 는 Job 이 만든 종료 pod 의 첫 컨테이너 terminated.Message(/dev/termination-log)를 읽는다.
func (o *MigObserver) terminationMessage(ctx context.Context, name string) (string, error) {
	var pods corev1.PodList
	if err := o.c.List(ctx, &pods, client.InNamespace(o.namespace), client.MatchingLabels{"job-name": name}); err != nil {
		return "", err
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				return cs.State.Terminated.Message, nil
			}
		}
	}
	return "", fmt.Errorf("no terminated pod message for job %q", name)
}

// failClosed 는 각 pci 에 대해 Err 를 채운 Unknown Observation 을 만든다(관측 실패시 apply 차단 신호).
func failClosed(pcis []string, reason string) []Observation {
	out := make([]Observation, 0, len(pcis))
	for _, pci := range pcis {
		out = append(out, Observation{PCI: pci, ModeCurrent: modeUnknown, ModePending: modeUnknown, Err: reason})
	}
	return out
}

// parseDelimitedMessage 는 Job 이 termination-log 에 쓴 구분자 포맷을 pci 별 Observation 으로 파싱한다.
// pci 마다 하나씩 반환하며, section 이 없으면 Err 를 채운다(조용히 드롭 금지, §16.3 fail-closed).
func parseDelimitedMessage(msg string, pcis []string) []Observation {
	sections := map[string]string{}
	var curPCI string
	buf := make([]string, 0, 16)
	flush := func() {
		if curPCI != "" {
			sections[curPCI] = strings.Join(buf, "\n")
		}
		buf = buf[:0]
	}
	for _, line := range strings.Split(msg, "\n") {
		if p, ok := parsePCIHeader(line); ok {
			flush()
			curPCI = p
			continue
		}
		buf = append(buf, line)
	}
	flush()

	out := make([]Observation, 0, len(pcis))
	for _, pci := range pcis {
		block, ok := sections[pci]
		if !ok {
			out = append(out, Observation{PCI: pci, ModeCurrent: modeUnknown, ModePending: modeUnknown, Err: "missing PCI section in observe output"})
			continue
		}
		mode := sectionBetween(block, "<<<MODE", ">>>MODE")
		lgi := sectionBetween(block, "<<<LGI", ">>>LGI")
		lgip := sectionBetween(block, "<<<LGIP", ">>>LGIP")
		out = append(out, parseObservation(pci, mode, lgi, lgip))
	}
	return out
}

// parsePCIHeader 는 "===PCI <pci>===" 행에서 pci 를 추출한다.
func parsePCIHeader(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "===PCI ") || !strings.HasSuffix(s, "===") {
		return "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(s, "===PCI "), "===")
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return "", false
	}
	return inner, true
}

// sectionBetween 은 start 마커 이후 ~ end 마커 이전의 내용을 trim 해 반환한다(없으면 "").
// LGI/LGIP 는 접두 충돌(<<<LGI ⊂ <<<LGIP)이 있으나, LGI 가 항상 LGIP 보다 먼저 나와 첫 매칭이 정답이다.
func sectionBetween(block, start, end string) string {
	i := strings.Index(block, start)
	if i < 0 {
		return ""
	}
	rest := block[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// renderObserveJob 은 각 pci 에 nsenter nvidia-smi 질의를 실행하고 구분자 포맷을 termination-log 에
// 쓰는 privileged/hostPID 1회성 Job 을 빌드한다. set -e 미사용 — nvidia-smi 실패(2>&1)도 텍스트로
// 캡처해 parse 가 fail-closed 판정하게 한다.
func renderObserveJob(name, nodeName string, pcis []string, image, namespace string) *batchv1.Job {
	ttl := migJobTTLSeconds
	backoff := migJobBackoffLimit
	labels := map[string]string{
		"app.kubernetes.io/name":      "kcloud-acpp-mig",
		"app.kubernetes.io/component": "mig-observe",
		"npu.ai/node":                 nodeName,
	}
	podSpec := corev1.PodSpec{
		NodeName:      nodeName,
		HostPID:       true,
		RestartPolicy: corev1.RestartPolicyNever,
		Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
		Containers: []corev1.Container{{
			Name:                     "mig-observe",
			Image:                    image,
			ImagePullPolicy:          corev1.PullIfNotPresent,
			Command:                  []string{"/bin/sh", "-c", observeScript(pcis)},
			SecurityContext:          &corev1.SecurityContext{Privileged: boolPtr(true)},
			TerminationMessagePath:   "/dev/termination-log",
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		}},
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}
}

// observeScript 는 각 pci 에 대해 MODE/LGI/LGIP 섹션을 구분자로 감싸 termination-log 에 쓴다.
// multi-line -lgi/-lgip 를 위해 <<<SEC ... >>>SEC 마커로 감싸고, echo 로 마커가 항상 자기 행에
// 오도록 보장한다. 전체 루프 출력을 termination-log 로 리다이렉트(4096B 한도 내, 보통 1 device).
func observeScript(pcis []string) string {
	var b strings.Builder
	b.WriteString("for pci in")
	for _, p := range pcis {
		b.WriteString(" '" + p + "'")
	}
	b.WriteString("; do\n")
	b.WriteString("printf '===PCI %s===\\n' \"$pci\"\n")
	b.WriteString("printf '<<<MODE\\n'\n")
	b.WriteString("nsenter --target 1 --mount --pid -- nvidia-smi -i \"$pci\" --query-gpu=mig.mode.current,mig.mode.pending --format=csv,noheader 2>&1\n")
	b.WriteString("echo; printf '>>>MODE\\n'\n")
	b.WriteString("printf '<<<LGI\\n'\n")
	b.WriteString("nsenter --target 1 --mount --pid -- nvidia-smi mig -i \"$pci\" -lgi 2>&1\n")
	b.WriteString("echo; printf '>>>LGI\\n'\n")
	b.WriteString("printf '<<<LGIP\\n'\n")
	b.WriteString("nsenter --target 1 --mount --pid -- nvidia-smi mig -i \"$pci\" -lgip 2>&1\n")
	b.WriteString("echo; printf '>>>LGIP\\n'\n")
	b.WriteString("done > /dev/termination-log\n")
	return b.String()
}
